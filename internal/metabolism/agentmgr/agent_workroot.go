package agentmgr

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/internal/sandbox"
)

// Work-root ownership and the boot sweep (ENG-2088,
// T-20261118-agent-scratch-and-socket-directories-survive-a).
//
// An instance's scratch (<work_root>/<tier>/<agent>-<inst8>) and socket
// directory (<work_root>/sock/<inst8>) are removed when the instance exits.
// A kernel that is killed or crashes never gets there, and its instances'
// directories — home, tmp and uncollected out/ files — stayed on disk for
// good. So every kernel OWNS the instance directories it creates, and at boot,
// before any agent spawns, removes those no live kernel owns:
//
//   - <work_root>/owners/<id>.lock is this kernel's lock file, held with an
//     exclusive OS lock (LockFileEx, flock) for the kernel's life; the OS drops
//     the lock when the process dies, however it dies;
//   - <work_root>/owners/<id>/ holds one empty marker per instance directory
//     the kernel has, named by the directory's path below the work root with
//     "/" as "~" (neither tier names nor sandbox server ids contain "~"). The
//     marker is written BEFORE the directory is created and removed after it
//     is gone;
//   - the sweep lists the instance directories FIRST, then reads the markers of
//     every owner whose lock another holder has (live). With marker-before-
//     mkdir, a directory the listing saw is already claimed when the markers
//     are read, so a second live kernel sharing the work root never loses one.
//     An owner whose lock the sweep can take is dead: its records go.
//
// Orphaned out/ files are deleted, not collected into the ledger: their run,
// session and instance provenance died with the owning kernel, and the files
// are agent-written, so a collection would mint artifacts with no trustworthy
// writer. ADR-0157 D5.1: outputs of an instance that dies with the kernel are
// lost, never leaked. The sweep logs how many it discarded.

const (
	ownersDirName = "owners"
	lockSuffix    = ".lock"
	markerSep     = "~"
)

// sweptTierDirs are the work-root subdirectories holding instance
// directories: one per launch tier, plus the socket directories.
var sweptTierDirs = []string{string(sandbox.TierFirstParty), string(sandbox.TierThirdParty), "sock"}

// WorkRootSweep is what OwnWorkRoot found and removed.
type WorkRootSweep struct {
	// Removed are the orphaned instance directories removed (relative to the
	// work root, slash-separated, sorted).
	Removed []string
	// OutFiles is how many uncollected out/ files they held (discarded).
	OutFiles int
	// LiveOwners counts the other kernels holding the work root; DeadOwners
	// the dead kernels whose records were cleared.
	LiveOwners, DeadOwners int
	// Failed names directories that could not be removed ("path: error").
	Failed []string
	// Skipped says why unclaimed directories were left alone ("" = swept).
	Skipped string
}

// workRootOwner is this kernel's claim on the agent work root.
type workRootOwner struct {
	mu      sync.Mutex
	root    string
	id      string
	lock    *os.File
	markers string // <root>/owners/<id>
}

func newOwnerID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// acquireWorkRoot creates and locks a fresh owner under root/owners. The lock
// file is checked to still be the file at its path after locking: a
// concurrent sweep may have taken a just-created, not yet locked file for a
// dead owner's and unlinked it (Unix), and a lock on an unlinked file would
// protect nothing.
func acquireWorkRoot(root string) (*workRootOwner, error) {
	owners := filepath.Join(root, ownersDirName)
	if err := os.MkdirAll(owners, 0o700); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		id, err := newOwnerID()
		if err != nil {
			return nil, err
		}
		path := filepath.Join(owners, id+lockSuffix)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		held, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if held {
			// A concurrent sweep took the fresh, not yet locked file for a dead
			// owner's; it is about to remove it. Start over with a new id.
			_ = f.Close()
			continue
		}
		fi, ferr := f.Stat()
		pi, perr := os.Stat(path)
		if ferr != nil || perr != nil || !os.SameFile(fi, pi) {
			_ = f.Close()
			continue
		}
		o := &workRootOwner{root: root, id: id, lock: f, markers: filepath.Join(owners, id)}
		if err := os.MkdirAll(o.markers, 0o700); err != nil {
			o.close()
			return nil, err
		}
		return o, nil
	}
	return nil, errors.New("could not keep a lock file in place under " + owners)
}

func markerName(rel string) string { return strings.ReplaceAll(filepath.ToSlash(rel), "/", markerSep) }
func markerRel(name string) string { return strings.ReplaceAll(name, markerSep, "/") }

// relDir is dir below the work root, slash-separated ("" when outside it).
func (o *workRootOwner) relDir(dir string) string {
	rel, err := filepath.Rel(o.root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// claim records the instance directories (absolute paths under the work root)
// as this kernel's, before they are created. nil-safe: a sandbox that does
// not own its work root (tests) claims nothing.
func (o *workRootOwner) claim(dirs ...string) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range dirs {
		rel := o.relDir(d)
		if rel == "" {
			return fmt.Errorf("instance directory %s is not under the agent work root %s", d, o.root)
		}
		if err := os.WriteFile(filepath.Join(o.markers, markerName(rel)), nil, 0o600); err != nil {
			return fmt.Errorf("claim %s: %w", rel, err)
		}
	}
	return nil
}

// release drops the claims on dirs, once they are gone (nil-safe).
func (o *workRootOwner) release(dirs ...string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range dirs {
		if rel := o.relDir(d); rel != "" {
			_ = os.Remove(filepath.Join(o.markers, markerName(rel))) // best effort: a stale marker only delays a sweep
		}
	}
}

// close gives the work root up: the markers directory goes when no claim is
// left (a claim still there — an instance whose cleanup has not run — makes
// the next boot treat this owner as dead and sweep it), then the lock.
func (o *workRootOwner) close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lock == nil {
		return
	}
	_ = os.Remove(o.markers) // fails while claims remain; then the next boot sweeps them
	path := o.lock.Name()
	_ = o.lock.Close() // releases the OS lock
	o.lock = nil
	_ = os.Remove(path)
}

// ownerLock is one entry of <root>/owners: a lock file or a markers directory.
type ownerRecord struct {
	id      string
	hasLock bool
	hasDir  bool
}

func listOwners(owners string) (map[string]*ownerRecord, error) {
	ents, err := os.ReadDir(owners)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]*ownerRecord{}
	get := func(id string) *ownerRecord {
		if out[id] == nil {
			out[id] = &ownerRecord{id: id}
		}
		return out[id]
	}
	for _, e := range ents {
		switch {
		case e.IsDir():
			get(e.Name()).hasDir = true
		case strings.HasSuffix(e.Name(), lockSuffix):
			get(strings.TrimSuffix(e.Name(), lockSuffix)).hasLock = true
		}
	}
	return out, nil
}

// listInstanceDirs lists every instance directory under the swept
// subdirectories (slash-separated, relative to root).
func listInstanceDirs(root string) []string {
	var out []string
	for _, sub := range sweptTierDirs {
		ents, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				out = append(out, sub+"/"+e.Name())
			}
		}
	}
	return out
}

func readMarkers(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, markerRel(e.Name()))
		}
	}
	return out
}

// countFiles counts the regular files under dir (0 when it is absent).
func countFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

// sweep removes the instance directories under o.root that no live owner
// claims (only those a dead owner recorded when sweepUnclaimed is false) and
// clears dead owners' records. o is this kernel's fresh owner: it has no
// claims yet, so the sweep runs before any agent of this kernel spawns.
func (o *workRootOwner) sweep(sweepUnclaimed bool) (WorkRootSweep, error) {
	var rep WorkRootSweep
	// 1. The instance directories, BEFORE any owner's markers (see the
	// package comment for why this order keeps a live kernel's directories).
	candidates := listInstanceDirs(o.root)
	// 2. The owners: live ones claim directories, dead ones are cleared.
	owners := filepath.Join(o.root, ownersDirName)
	recs, err := listOwners(owners)
	if err != nil {
		return rep, err
	}
	claimed := map[string]bool{}
	deadClaims := map[string]bool{}
	type dead struct {
		rec  *ownerRecord
		lock *os.File
	}
	var deads []dead
	for _, r := range recs {
		if r.id == o.id {
			continue
		}
		if !r.hasLock {
			// A markers directory without a lock file: its kernel is gone
			// (the lock file is removed only after the OS lock is dropped).
			for _, c := range readMarkers(filepath.Join(owners, r.id)) {
				deadClaims[c] = true
			}
			deads = append(deads, dead{rec: r})
			continue
		}
		f, err := os.OpenFile(filepath.Join(owners, r.id+lockSuffix), os.O_RDWR, 0)
		if err != nil {
			// Removed since the listing (its kernel just gave the root up) or
			// unreadable: treat it as live, never delete what it may claim.
			for _, c := range readMarkers(filepath.Join(owners, r.id)) {
				claimed[c] = true
			}
			rep.LiveOwners++
			continue
		}
		held, lerr := tryLockFile(f)
		if lerr != nil || held {
			_ = f.Close()
			for _, c := range readMarkers(filepath.Join(owners, r.id)) {
				claimed[c] = true
			}
			rep.LiveOwners++
			continue
		}
		for _, c := range readMarkers(filepath.Join(owners, r.id)) {
			deadClaims[c] = true
		}
		deads = append(deads, dead{rec: r, lock: f})
	}
	// 3. Remove the orphans.
	var remove []string
	if sweepUnclaimed {
		for _, c := range candidates {
			if !claimed[c] {
				remove = append(remove, c)
			}
		}
	} else {
		for _, c := range candidates {
			if deadClaims[c] && !claimed[c] {
				remove = append(remove, c)
			}
		}
	}
	sort.Strings(remove)
	for _, rel := range remove {
		dir := filepath.Join(o.root, filepath.FromSlash(rel))
		outs := countFiles(filepath.Join(dir, AgentOutDir))
		if err := os.RemoveAll(dir); err != nil {
			rep.Failed = append(rep.Failed, rel+": "+err.Error())
			continue
		}
		rep.Removed = append(rep.Removed, rel)
		rep.OutFiles += outs
	}
	// 4. Clear the dead owners' records (markers first, then the lock file,
	// which on Windows cannot be removed while open).
	for _, d := range deads {
		_ = os.RemoveAll(filepath.Join(owners, d.rec.id))
		if d.lock != nil {
			_ = d.lock.Close()
		}
		if d.rec.hasLock {
			_ = os.Remove(filepath.Join(owners, d.rec.id+lockSuffix))
		}
	}
	rep.DeadOwners = len(deads)
	return rep, nil
}

// overlaps reports whether a and b are the same directory or one holds the
// other.
func overlaps(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if abs, err := filepath.Abs(a); err == nil {
		a = abs
	}
	if abs, err := filepath.Abs(b); err == nil {
		b = abs
	}
	return pathWithin(a, b) || pathWithin(b, a)
}

func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// OwnWorkRoot makes this kernel an owner of the agent work root and sweeps
// the instance directories no live kernel owns (ENG-2088). The composition
// root calls it once at boot, before any agent spawns; from then on every
// sandboxed launch claims its scratch and socket directory before creating
// them. keep are other work roots that must never be swept (the MCP
// sandbox's, whose <root>/<tier>/<id> workdirs look like agent scratch): when
// one overlaps the agent work root, only directories a dead agent kernel
// recorded are removed. Calling it again returns the first owner's result
// with no sweep.
func (s *AgentSandbox) OwnWorkRoot(keep ...string) (WorkRootSweep, error) {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	if s.owner != nil {
		return WorkRootSweep{Skipped: "already owned"}, nil
	}
	root := s.policy.WorkRoot
	if err := os.MkdirAll(root, 0o711); err != nil {
		return WorkRootSweep{}, err
	}
	o, err := acquireWorkRoot(root)
	if err != nil {
		return WorkRootSweep{}, err
	}
	sweepUnclaimed := true
	var skipped string
	for _, k := range keep {
		if overlaps(k, root) {
			sweepUnclaimed = false
			skipped = "the work root overlaps " + k + "; only directories a dead agent kernel recorded were removed"
		}
	}
	rep, err := o.sweep(sweepUnclaimed)
	rep.Skipped = skipped
	s.owner = o
	return rep, err
}

// ReleaseWorkRoot gives the work root up at shutdown (nil-safe).
func (s *AgentSandbox) ReleaseWorkRoot() {
	if s == nil {
		return
	}
	s.ownerMu.Lock()
	o := s.owner
	s.owner = nil
	s.ownerMu.Unlock()
	o.close()
}

// workRootOwner is the sandbox's owner, nil until OwnWorkRoot.
func (s *AgentSandbox) workRootOwner() *workRootOwner {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return s.owner
}
