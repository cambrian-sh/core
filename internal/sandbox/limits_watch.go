package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The agent limit watch (ENG-2112). The OS ceilings stop a runaway agent from
// GROWING past a limit, but most of them only make the next allocation or
// fork fail, and an agent that swallows the error keeps its instance alive at
// the ceiling — a fork-bombing or disk-filling daemon worker would sit there
// for its whole life. Owner decision 2026-11-18: exceeding a limit KILLS the
// instance, into the existing restart/quarantine policy, with a clear reason.
//
// What kills, per limit:
//   - memory: the Windows job (JOB_OBJECT_MSG_JOB_MEMORY_LIMIT, start_windows.go);
//     on Linux the cgroup's OOM kill, else this watch (tree RSS over the cap);
//   - processes: the Windows job (JOB_OBJECT_MSG_ACTIVE_PROCESS_LIMIT, agents
//     only); on Linux this watch (the tree's process count reaching the cap —
//     the hard ceiling below it, MaxTasks, stops the fork itself);
//   - scratch_quota: this watch on every OS (size accounting on the workdir).
//
// One goroutine per agent launch that has something to watch, owned by its
// Launched: started by StartCmd, stopped by Release (goleak-tested).

// defaultLimitPoll is the watch's sampling period when Policy.LimitPoll is 0.
// A second bounds what an agent can write past its quota to one second of
// disk bandwidth, and costs one directory walk of a scratch directory.
const defaultLimitPoll = time.Second

type limitWatch struct {
	stop chan struct{}
	done chan struct{}

	mu     sync.Mutex
	reason string
}

// treeSample is one look at a launched process tree: its process count and
// resident memory. ok=false where the OS has no cheap way to see the tree.
type treeSample struct {
	procs int
	rss   int64
	ok    bool
}

// startLimitWatch starts the watch for one launch, or returns nil when there
// is nothing for it to watch on this OS. kill stops the whole tree.
func startLimitWatch(tp TierPolicy, workdir string, pid int, interval time.Duration, kill func() error) *limitWatch {
	watchScratch := tp.ScratchMB > 0 && workdir != ""
	watchTree := treeWatchable && (tp.MaxProcesses > 0 || tp.MemoryMB > 0) && pid > 0
	if !watchScratch && !watchTree {
		return nil
	}
	if interval <= 0 {
		interval = defaultLimitPoll
	}
	w := &limitWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
			}
			if r := checkLimits(tp, workdir, pid, watchScratch, watchTree); r != "" {
				w.setReason(r)
				_ = kill()
				return
			}
		}
	}()
	return w
}

// checkLimits samples once and returns why the tree must die ("" if it may live).
func checkLimits(tp TierPolicy, workdir string, pid int, scratch, tree bool) string {
	if scratch {
		if n := dirSize(workdir); n > int64(tp.ScratchMB)<<20 {
			return fmt.Sprintf("scratch_quota: the instance's scratch reached %d MiB (limit %d MiB)", n>>20, tp.ScratchMB)
		}
	}
	if tree {
		s := sampleTree(pid)
		if s.ok && tp.MaxProcesses > 0 && s.procs >= tp.MaxProcesses {
			return fmt.Sprintf("processes: the process tree reached its %d-process limit (%d running)", tp.MaxProcesses, s.procs)
		}
		if s.ok && tp.MemoryMB > 0 && s.rss > int64(tp.MemoryMB)<<20 {
			return fmt.Sprintf("memory: the process tree reached its %d MiB limit (%d MiB resident)", tp.MemoryMB, s.rss>>20)
		}
	}
	return ""
}

// dirSize is the total size of the regular files under dir. Unreadable
// entries are skipped: the watch measures, it never fails the launch.
//
// Each file is stat'ed by path, not sized from the directory listing: on
// Windows (NTFS) the listing's size of a file still held open is the size at
// its last close, so an agent that keeps its fill file open would grow it
// unseen (found by the ENG-2112 e2e: a 512 MiB fill read as small).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, serr := os.Lstat(path); serr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (w *limitWatch) setReason(r string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reason == "" {
		w.reason = r
	}
}

// killReason is why the watch killed the tree ("" if it did not). nil-safe.
func (w *limitWatch) killReason() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// close stops the watch and waits for its goroutine. nil-safe, idempotent
// through Launched.Release's once.
func (w *limitWatch) close() {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.done
}
