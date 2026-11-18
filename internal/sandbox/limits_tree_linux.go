//go:build linux

package sandbox

import (
	"os"
	"strconv"
	"strings"
)

// treeWatchable: Linux exposes every process's parent and resident size in
// /proc, so the limit watch can count a launched tree's processes (not its
// threads: /proc lists thread-group leaders) and sum its memory.
const treeWatchable = true

// sampleTree walks /proc once and measures the tree rooted at pid (the
// wrapper the runner started: sh, bwrap or sandbox-exec), root included.
func sampleTree(pid int) treeSample {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return treeSample{}
	}
	parent := map[int]int{}
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pp, ok := procPPID(p); ok {
			parent[p] = pp
		}
	}
	if _, ok := parent[pid]; !ok {
		return treeSample{}
	}
	inTree := func(p int) bool {
		for i := 0; i < 64 && p > 1; i++ { // a bounded walk: no cycle, no deep chain
			if p == pid {
				return true
			}
			p = parent[p]
		}
		return p == pid
	}
	s := treeSample{ok: true}
	page := int64(os.Getpagesize())
	for p := range parent {
		if !inTree(p) {
			continue
		}
		s.procs++
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/statm"); err == nil {
			if f := strings.Fields(string(raw)); len(f) > 1 {
				if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					s.rss += n * page
				}
			}
		}
	}
	return s
}

// procPPID reads a process's parent from /proc/<pid>/stat. The command name
// field is parenthesised and may hold spaces, so the fields after it are
// counted from the last ')'.
func procPPID(pid int) (int, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	pp, err := strconv.Atoi(f[1])
	return pp, err == nil
}
