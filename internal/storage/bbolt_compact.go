package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"go.etcd.io/bbolt"
)

// Offline compaction of a bbolt file (ADR-0061 A2.8).
//
// bbolt reuses freed pages but never returns them to the file system: the file
// stays at its high-water mark however much is deleted. On 2026-11-02
// data/agents.db was 3.2 GB holding 17 MB of live data, because a journal flood
// had been written and (by hand) deleted. A working GC bounds the LIVE data; only
// a rewrite shrinks the FILE, and bbolt cannot rewrite a file it has open — so
// compaction runs before the store is opened.

// PreCompactSuffix names the previous file kept after a compaction swap. It stays
// until the next boot that opens the store without compacting, so a compaction
// that produced a bad file can be undone by hand.
const PreCompactSuffix = ".pre-compact"

// compactingSuffix names the in-progress copy; a leftover one is from a run that
// died before the swap and is discarded.
const compactingSuffix = ".compacting"

// compactTxMaxSize bounds each copy transaction in bbolt.Compact.
const compactTxMaxSize = 64 << 20

// CompactReport describes one inspection and, when it ran, one compaction.
type CompactReport struct {
	Path string
	// FileBytes is the file size before; LiveBytes is the in-use page estimate
	// (high-water mark minus free pages); FreeRatio is 1 - LiveBytes/FileBytes.
	FileBytes int64
	LiveBytes int64
	FreeRatio float64
	// Compacted is true when the file was rewritten; AfterBytes is its new size.
	Compacted  bool
	AfterBytes int64
	Duration   time.Duration
	// Reason says why it did or did not compact, for the boot log.
	Reason string
}

// InspectBBoltFile reports a bbolt file's size and how much of it is free pages,
// without modifying it. A missing file reports zero sizes and no error. The file
// must not be open elsewhere (bbolt's file lock times out after one second).
func InspectBBoltFile(path string) (CompactReport, error) {
	rep := CompactReport{Path: path}
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		rep.Reason = "no file"
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("compact: stat %s: %w", path, err)
	}
	rep.FileBytes = st.Size()
	if rep.FileBytes == 0 {
		rep.Reason = "empty file"
		return rep, nil
	}
	// ReadOnly + PreLoadFreelist: the freelist is what says how much of the file
	// is reusable, and a read-only open skips loading it unless asked.
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, PreLoadFreelist: true, Timeout: time.Second})
	if err != nil {
		return rep, fmt.Errorf("compact: open %s: %w", path, err)
	}
	defer db.Close()
	tx, err := db.Begin(false)
	if err != nil {
		return rep, fmt.Errorf("compact: read tx: %w", err)
	}
	highWater := tx.Size()
	_ = tx.Rollback() // read-only transaction; nothing to commit
	free := int64(db.Stats().FreePageN) * int64(db.Info().PageSize)
	rep.LiveBytes = highWater - free
	if rep.LiveBytes < 0 {
		rep.LiveBytes = 0
	}
	rep.FreeRatio = 1 - float64(rep.LiveBytes)/float64(rep.FileBytes)
	return rep, nil
}

// CompactBBoltFileIfBloated compacts path when it is larger than minBytes AND
// more than minFreeRatio of it is free (A2.8; the kernel passes
// storage.compact_min_bytes and storage.compact_free_ratio). minBytes <= 0
// disables compaction. Both thresholds must hold: a small file is not worth a
// boot-time rewrite however empty it is, and a large file that is mostly live
// data would be rewritten for little gain.
func CompactBBoltFileIfBloated(path string, minBytes int64, minFreeRatio float64) (CompactReport, error) {
	if minBytes <= 0 {
		return CompactReport{Path: path, Reason: "disabled (compact_min_bytes <= 0)"}, nil
	}
	rep, err := InspectBBoltFile(path)
	if err != nil || rep.FileBytes == 0 {
		return rep, err
	}
	if rep.FileBytes <= minBytes {
		rep.Reason = fmt.Sprintf("file %d B is not above the %d B threshold", rep.FileBytes, minBytes)
		return rep, nil
	}
	if rep.FreeRatio <= minFreeRatio {
		rep.Reason = fmt.Sprintf("free ratio %.2f is not above %.2f", rep.FreeRatio, minFreeRatio)
		return rep, nil
	}
	return compactInto(rep)
}

// CompactBBoltFile compacts path unconditionally (the maintenance command).
func CompactBBoltFile(path string) (CompactReport, error) {
	rep, err := InspectBBoltFile(path)
	if err != nil {
		return rep, err
	}
	if rep.FileBytes == 0 {
		return rep, nil
	}
	return compactInto(rep)
}

// compactInto rewrites rep.Path into a fresh file and swaps it in, keeping the
// original as <path>.pre-compact. On any failure before the swap the original is
// untouched; a failure in the swap itself restores it.
func compactInto(rep CompactReport) (CompactReport, error) {
	start := time.Now()
	path := rep.Path
	tmp := path + compactingSuffix
	pre := path + PreCompactSuffix
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rep, fmt.Errorf("compact: remove stale %s: %w", tmp, err)
	}

	src, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return rep, fmt.Errorf("compact: open source: %w", err)
	}
	dst, err := bbolt.Open(tmp, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		_ = src.Close()
		return rep, fmt.Errorf("compact: open destination: %w", err)
	}
	cerr := bbolt.Compact(dst, src, compactTxMaxSize)
	dcerr := dst.Close()
	_ = src.Close() // read-only; closing releases the shared lock
	if cerr == nil {
		cerr = dcerr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return rep, fmt.Errorf("compact: copy: %w", cerr)
	}

	if err := os.Remove(pre); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(tmp)
		return rep, fmt.Errorf("compact: remove previous %s: %w", pre, err)
	}
	if err := os.Rename(path, pre); err != nil {
		_ = os.Remove(tmp)
		return rep, fmt.Errorf("compact: keep original as %s: %w", pre, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if rerr := os.Rename(pre, path); rerr != nil {
			return rep, fmt.Errorf("compact: swap failed (%v) AND restoring the original failed (%v); the original is at %s", err, rerr, pre)
		}
		_ = os.Remove(tmp)
		return rep, fmt.Errorf("compact: swap: %w", err)
	}
	if st, err := os.Stat(path); err == nil {
		rep.AfterBytes = st.Size()
	}
	rep.Compacted = true
	rep.Duration = time.Since(start)
	rep.Reason = fmt.Sprintf("compacted (free ratio %.2f)", rep.FreeRatio)
	return rep, nil
}

// RemoveStalePreCompact deletes <path>.pre-compact, the file a previous
// compaction kept. The kernel calls it after a boot that opened the store without
// compacting: that boot is the "next successful boot" A2.8 keeps the file until.
func RemoveStalePreCompact(path string) (removed bool, err error) {
	err = os.Remove(path + PreCompactSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
