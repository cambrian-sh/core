// bbolt-compact rewrites the kernel's bbolt store into a fresh file, returning
// freed pages to the file system (ADR-0061 Amendment A2.8).
//
// bbolt reuses freed pages but never shrinks the file: after a large backlog is
// pruned, the file stays at its high-water mark (data/agents.db was 3.2 GB for
// 17 MB of live data on 2026-11-02). The kernel compacts automatically at boot
// when the file is over storage.compact_min_bytes and more than
// storage.compact_free_ratio of it is free; this command is the same operation on
// demand, for an operator who does not want to wait for a restart past the
// thresholds.
//
// The kernel must be stopped: bbolt holds an exclusive file lock while the store
// is open, and this tool refuses (after a one-second lock timeout) rather than
// waiting. The previous file is kept as <db>.pre-compact; the kernel removes it on
// its next boot that does not compact.
//
// It touches only the bbolt file — no vector store, no MemoryStack (AGENTS.md §2
// governs principal-serving memory access; this is offline maintenance of the
// kernel's own registry file).
//
// Usage:
//
//	bbolt-compact -db data/agents.db            # report the free ratio only
//	bbolt-compact -db data/agents.db -apply     # compact unconditionally
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cambrian-sh/core/internal/storage"
)

func main() {
	dbPath := flag.String("db", "data/agents.db", "path to the bbolt file")
	apply := flag.Bool("apply", false, "compact the file (default: report only)")
	flag.Parse()

	if !*apply {
		rep, err := storage.InspectBBoltFile(*dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bbolt-compact:", err)
			os.Exit(1)
		}
		fmt.Printf("%s: file %d B, live ~%d B, free ratio %.2f (report only; -apply to compact)\n",
			rep.Path, rep.FileBytes, rep.LiveBytes, rep.FreeRatio)
		return
	}
	rep, err := storage.CompactBBoltFile(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bbolt-compact:", err)
		os.Exit(1)
	}
	if !rep.Compacted {
		fmt.Printf("%s: nothing to compact (%s)\n", rep.Path, rep.Reason)
		return
	}
	fmt.Printf("%s: compacted %d -> %d B in %s; previous file kept as %s%s\n",
		rep.Path, rep.FileBytes, rep.AfterBytes, rep.Duration, rep.Path, storage.PreCompactSuffix)
}
