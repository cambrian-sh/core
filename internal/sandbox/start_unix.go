//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Unix native containment:
//
//   - every tier: the process starts through `/bin/sh -c 'ulimit …; exec "$@"'`
//     so the limits are in place BEFORE the server's first instruction (Go has
//     no pre-exec hook): -d (data segment: memory; per process), -t (CPU
//     seconds; per process). The child leads its own process group and gets
//     SIGKILL if the kernel dies (Linux Pdeathsig); stopping it kills the group.
//   - a delegated cgroup v2 parent (Linux, mcp.sandbox.cgroup_parent): each
//     server gets its own cgroup with memory.max and pids.max, joined at clone
//     time (SysProcAttr.UseCgroupFD), and is stopped through cgroup.kill. This
//     is the only native process-count ceiling outside a user namespace.
//   - bubblewrap (Linux, when installed and usable) for fs_confine and network:
//     the root is bound read-only, the workdir (and a declared root) read-write,
//     /tmp private, mcp.sandbox.hide_paths masked, new pid/ipc/uts (and, unless
//     egress is declared, net) namespaces, --die-with-parent. Unprivileged it
//     also unshares the user namespace, where `ulimit -u` counts only the
//     sandbox's own processes.
//   - low_privilege: only when the kernel runs as root, the process (or bwrap)
//     runs as nobody (65534). A non-root kernel has no lower identity to drop
//     to; that tier needs runner docker.
//   - macOS / BSD: the sh ulimit limits and the process group only.
func nativeCapability(p *Policy, spec Spec, tp TierPolicy) capability {
	c := capability{
		runner: RunnerNative,
		enforced: map[Control]string{
			ControlEnv: "", ControlWorkdir: "", ControlOutputCap: "", ControlStallKill: "",
			ControlKillTree: "process_group", ControlMemory: "ulimit", ControlCPUTime: "ulimit",
		},
		missing: map[Control]string{},
	}
	if ok, why := cgroupUsable(p.CgroupParent); ok {
		c.enforced[ControlMemory] = "cgroup"
		c.enforced[ControlProcesses] = "cgroup"
		c.enforced[ControlKillTree] = "cgroup"
	} else {
		if tp.MemoryMB > 0 {
			c.notes = append(c.notes, "memory: per-process data-segment limit (ulimit -d), not a tree total")
		}
		c.missing[ControlProcesses] = "needs a delegated cgroup v2 parent (mcp.sandbox.cgroup_parent: " + why + ") or bubblewrap with a user namespace"
	}
	bw, bwWhy := bwrapUsable(p.BwrapPath)
	useBwrap := bw != "" && wantsBwrap(spec, tp)
	if bw != "" {
		c.enforced[ControlFSConfine] = "bwrap"
		c.enforced[ControlNetwork] = "bwrap"
		if useBwrap && (os.Geteuid() != 0 || tp.LowPrivilege) {
			if _, ok := c.enforced[ControlProcesses]; !ok {
				c.enforced[ControlProcesses] = "ulimit"
				delete(c.missing, ControlProcesses)
			}
		}
		c.enforced[ControlHideState] = "bwrap"
		if tp.ConfineFS {
			c.notes = append(c.notes, "fs: root read-only, writes only to the workdir (seen at "+bwrapWork+")"+rootNote(spec)+"; hide_paths masked")
		}
	} else {
		c.missing[ControlFSConfine] = bwWhy
		c.missing[ControlNetwork] = bwWhy
		c.missing[ControlHideState] = bwWhy
	}
	if spec.Agent != nil && tp.DenyNetwork && bw != "" {
		c.notes = append(c.notes, "network: kernel-only (no network namespace; the kernel is reached through the unix socket in the socket directory)")
	}
	if spec.Agent != nil && tp.HideState && runtime.GOOS == "linux" {
		c.notes = append(c.notes, "fs: landlock and seccomp are not applied (bubblewrap mount namespace only)")
	}
	if os.Geteuid() == 0 {
		if why := traversableByOthers(p.WorkRoot); why != "" {
			c.missing[ControlLowPrivilege] = "uid 65534 could not reach its workdir: " + why +
				" (set mcp.sandbox.work_root outside /root, e.g. /var/lib/cambrian/sandbox)"
		} else {
			c.enforced[ControlLowPrivilege] = "uid"
		}
	} else {
		c.missing[ControlLowPrivilege] = "the kernel is not root, so there is no lower uid to switch to; use runner docker"
	}
	return c
}

func rootNote(spec Spec) string {
	if spec.Server.Root != "" {
		return " and " + spec.Server.Root
	}
	return ""
}

// wantsBwrap: bubblewrap is used only when a control it provides is wanted.
func wantsBwrap(spec Spec, tp TierPolicy) bool {
	return tp.ConfineFS || (tp.DenyNetwork && spec.Server.Network != NetworkEgress)
}

func startNative(p *Policy, spec Spec, tp TierPolicy, c Command, workdir string, start starter) error {
	bw, _ := bwrapUsable(p.BwrapPath)
	useBwrap := bw != "" && wantsBwrap(spec, tp)
	// Inside bubblewrap the workdir is mounted at bwrapWork, inside the
	// sandbox's private /tmp: binding it at its host path would make bwrap
	// create the mount point's parents, which an unprivileged sandbox cannot
	// do under a root-owned work_root.
	inner := workdir
	if useBwrap {
		inner = bwrapWork
	}
	env, err := workdirEnv(p, c.Env, tp.ConfineFS, workdir, inner, "/")
	if err != nil {
		return err
	}
	path, err := lookPathIn(c.Path, env)
	if err != nil {
		return err
	}
	if useBwrap {
		if rel, ok := strings.CutPrefix(path, workdir+"/"); ok {
			path = bwrapWork + "/" + rel
		}
	}
	root := os.Geteuid() == 0
	dropUID := root && tp.LowPrivilege
	if dropUID {
		_ = os.Chown(workdir, 65534, 65534)
		chownTree(workdir, 65534, 65534)
		if spec.Agent != nil && spec.Agent.SocketDir != "" {
			_ = os.Chown(spec.Agent.SocketDir, 65534, 65534)
		}
	}

	// sh -c 'ulimit …; exec "$@"' sh <path> <args…>
	var lim []string
	if tp.MemoryMB > 0 {
		lim = append(lim, "ulimit -d "+strconv.Itoa(tp.MemoryMB*1024))
	}
	if tp.CPUTimeSec > 0 {
		lim = append(lim, "ulimit -t "+strconv.Itoa(tp.CPUTimeSec))
	}
	// In bwrap's fresh user namespace RLIMIT_NPROC counts only the sandbox's
	// own processes; outside one it would count every process of the uid.
	unprivNS := useBwrap && (!root || dropUID)
	if tp.MaxProcesses > 0 && unprivNS {
		lim = append(lim, "ulimit -u "+strconv.Itoa(tp.MaxProcesses))
	}
	script := strings.Join(append(lim, `exec "$@"`), "; ")
	argv := append([]string{"/bin/sh", "-c", script, "sh", path}, c.Args...)
	if useBwrap {
		argv = append(bwrapArgs(bw, p, spec, tp, workdir, c.Dir, unprivNS), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // operator-configured command
	cmd.Env = env
	cmd.Dir = workdir
	if c.Dir != "" && !useBwrap {
		cmd.Dir = c.Dir // inside bubblewrap the directory is set by --chdir
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(attr)
	if dropUID {
		attr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
	}
	cg, cgErr := newCgroup(p.CgroupParent, spec.ServerID, tp)
	if cgErr != nil {
		return fmt.Errorf("sandbox: cgroup: %w", cgErr)
	}
	if cg != nil {
		cg.attach(attr)
	}
	cmd.SysProcAttr = attr
	post := func() (osHandle, error) {
		if cg != nil {
			cg.closeFD()
		}
		return &groupHandle{pid: cmd.Process.Pid, cg: cg}, nil
	}
	err = start(cmd, post)
	if err != nil && cg != nil {
		cg.closeFD()
		cg.remove()
	}
	return err
}

// bwrapWork is where a bubblewrap sandbox sees its workdir.
const bwrapWork = "/tmp/work"

func bwrapArgs(bw string, p *Policy, spec Spec, tp TierPolicy, workdir, dir string, userNS bool) []string {
	a := []string{bw, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	// ADR-0157 D1: an agent's binds outside the hide-list go first, so a hidden
	// path inside one stays masked; binds inside a hidden path (its code in the
	// kernel directory) go after the hides and re-expose only that subtree.
	var after []Bind
	if spec.Agent != nil {
		var before []Bind
		// StartCmd already dropped (and reported) a bind over the private /tmp;
		// dropped here too so no caller can lay one over it (ENG-1969).
		binds, _ := privateTmpSafe(spec.Agent.Binds)
		before, after, _ = splitBinds(binds, p.HidePaths)
		a = appendBinds(a, before)
	}
	var hiddenDirs []string
	for _, h := range p.HidePaths {
		if h == "" {
			continue
		}
		if fi, err := os.Stat(h); err == nil {
			if fi.IsDir() {
				a = append(a, "--tmpfs", h)
				hiddenDirs = append(hiddenDirs, h)
			} else {
				a = append(a, "--ro-bind", "/dev/null", h)
			}
		}
	}
	a = appendBinds(a, after)
	a = append(a, "--bind", workdir, bwrapWork)
	if spec.Agent != nil && spec.Agent.SocketDir != "" {
		// D2.6: the socket directory at its own path, so the --socket the kernel
		// passed and dials names the same file inside and outside.
		a = append(a, "--bind", spec.Agent.SocketDir, spec.Agent.SocketDir)
	}
	if spec.Server.Root != "" {
		if _, err := os.Stat(spec.Server.Root); err == nil {
			a = append(a, "--bind", spec.Server.Root, spec.Server.Root)
		}
	}
	if spec.Agent != nil {
		// An agent's hidden directories are empty AND read-only (ADR-0157 D1):
		// a write there must fail, not land in a throwaway tmpfs. Remounted
		// last, after the binds that re-expose subtrees inside them were made.
		for _, h := range hiddenDirs {
			a = append(a, "--remount-ro", h)
		}
	}
	chdir := bwrapWork
	if dir != "" {
		chdir = dir
	}
	a = append(a, "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session", "--chdir", chdir)
	if userNS {
		a = append(a, "--unshare-user")
	}
	if tp.DenyNetwork && spec.Server.Network != NetworkEgress {
		a = append(a, "--unshare-net")
	}
	return append(a, "--")
}

// appendBinds adds bubblewrap binds, each at its own path.
func appendBinds(a []string, binds []Bind) []string {
	for _, b := range binds {
		if b.Write {
			a = append(a, "--bind", b.Path, b.Path)
		} else {
			a = append(a, "--ro-bind", b.Path, b.Path)
		}
	}
	return a
}

// groupHandle kills the process group (or the cgroup) and removes the cgroup.
type groupHandle struct {
	pid  int
	cg   *cgroup
	once sync.Once
}

func (h *groupHandle) kill() error {
	if h.cg != nil && h.cg.kill() == nil {
		return nil
	}
	err := syscall.Kill(-h.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (h *groupHandle) release() {
	h.once.Do(func() {
		if h.cg != nil {
			// The cgroup can be removed only once empty; the processes were
			// just killed, so give the kernel a moment to reap them.
			for i := 0; i < 20 && h.cg.remove() != nil; i++ {
				time.Sleep(50 * time.Millisecond)
			}
		}
	})
}

var bwrapMemo struct {
	sync.Mutex
	m map[string][2]string
}

// bwrapUsable returns the bwrap path when bubblewrap is installed AND works
// here (a container without user namespaces has the binary but cannot use it),
// else "" and why.
func bwrapUsable(configured string) (string, string) {
	if runtime.GOOS != "linux" {
		return "", "bubblewrap is Linux-only; use runner docker on " + runtime.GOOS
	}
	bwrapMemo.Lock()
	defer bwrapMemo.Unlock()
	if r, ok := bwrapMemo.m[configured]; ok {
		return r[0], r[1]
	}
	path, why := probeBwrap(configured)
	if bwrapMemo.m == nil {
		bwrapMemo.m = map[string][2]string{}
	}
	bwrapMemo.m[configured] = [2]string{path, why}
	return path, why
}

func probeBwrap(configured string) (string, string) {
	name := configured
	if name == "" {
		name = "bwrap"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "bubblewrap (bwrap) is not installed"
	}
	args := []string{"--ro-bind", "/", "/", "--unshare-net", "--unshare-pid", "--die-with-parent"}
	if os.Geteuid() != 0 {
		args = append(args, "--unshare-user")
	}
	args = append(args, "--", "/bin/true")
	if out, err := exec.Command(path, args...).CombinedOutput(); err != nil { //nolint:gosec // probe of a configured binary
		return "", "bubblewrap is installed but cannot create namespaces here: " + strings.TrimSpace(string(out))
	}
	return path, ""
}

// lookPathIn resolves a bare command against the CHILD's PATH, because the
// sh wrapper (and bwrap) exec it by absolute path.
func lookPathIn(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			for _, dir := range strings.Split(kv[5:], ":") {
				if dir == "" {
					continue
				}
				cand := dir + "/" + name
				if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					return cand, nil
				}
			}
		}
	}
	return exec.LookPath(name)
}

func chownTree(dir string, uid, gid int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := dir + "/" + e.Name()
		_ = os.Lchown(p, uid, gid)
		if e.IsDir() {
			chownTree(p, uid, gid)
		}
	}
}

func (*groupHandle) limitReason() string { return "" }

// traversableByOthers reports why a process of another uid could not walk
// down to dir ("" if it can): every existing ancestor needs the o+x bit.
func traversableByOthers(dir string) string {
	if dir == "" {
		return "work_root is empty"
	}
	for d := dir; ; {
		if fi, err := os.Stat(d); err == nil && fi.Mode().Perm()&0o001 == 0 {
			return d + " is not traversable by other users"
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}
