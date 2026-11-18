//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func setPdeathsig(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }

// cgroupUsable reports whether parent is a delegated cgroup v2 directory this
// process can create children in, with the memory and pids controllers
// enabled for them.
func cgroupUsable(parent string) (bool, string) {
	if parent == "" {
		return false, "not configured"
	}
	ctl, err := os.ReadFile(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return false, "unreadable: " + err.Error()
	}
	have := strings.Fields(string(ctl))
	for _, want := range []string{"memory", "pids"} {
		found := false
		for _, h := range have {
			found = found || h == want
		}
		if !found {
			return false, want + " controller not enabled in " + parent
		}
	}
	if err := syscall.Access(parent, 0x2 /* W_OK */); err != nil {
		return false, "not writable: " + err.Error()
	}
	return true, ""
}

type cgroup struct {
	dir string
	fd  int
}

// newCgroup creates <parent>/cambrian-<id>-<rand> with the tier's limits, or
// returns nil when no usable parent is configured.
func newCgroup(parent, id string, tp TierPolicy) (*cgroup, error) {
	if ok, _ := cgroupUsable(parent); !ok {
		return nil, nil
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	dir := filepath.Join(parent, "cambrian-"+safeName(id)+"-"+hex.EncodeToString(rnd[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	write := func(name, v string) error { return os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644) }
	var err error
	if tp.MemoryMB > 0 {
		err = errors.Join(err, write("memory.max", strconv.Itoa(tp.MemoryMB<<20)), write("memory.swap.max", "0"))
	}
	if n := tp.taskCeiling(); n > 0 {
		err = errors.Join(err, write("pids.max", strconv.Itoa(n)))
	}
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	return &cgroup{dir: dir, fd: fd}, nil
}

// attach makes the child join the cgroup at clone time (no window outside it).
func (c *cgroup) attach(a *syscall.SysProcAttr) {
	a.UseCgroupFD = true
	a.CgroupFD = c.fd
}

func (c *cgroup) closeFD() {
	if c.fd >= 0 {
		_ = syscall.Close(c.fd)
		c.fd = -1
	}
}

// kill stops every process in the cgroup (cgroup.kill, Linux 5.14+).
func (c *cgroup) kill() error {
	return os.WriteFile(filepath.Join(c.dir, "cgroup.kill"), []byte("1"), 0o644)
}

func (c *cgroup) remove() error { return os.Remove(c.dir) }
