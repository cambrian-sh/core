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

	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
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
//     to; that tier needs runner docker. A launch uid 65534 could not boot
//     (lowUIDBootBlocker: a bind or hidden path under an ancestor it cannot
//     read) is refused at plan time, naming the path and the fix.
//   - macOS / BSD: the sh ulimit limits and the process group; on macOS an
//     AGENT additionally runs under a generated Seatbelt profile (sandbox-exec,
//     seatbelt.go; ADR-0157 S5) for fs_confine, hide_state and network.
func nativeCapability(p *Policy, spec Spec, tp TierPolicy, _ Command) capability {
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
		switch {
		case tp.MemoryMB > 0 && spec.Agent != nil && runtime.GOOS == "linux":
			// ENG-2112: the agent limit watch adds the tree total (limits_watch.go).
			c.notes = append(c.notes, "memory: per-process data-segment limit (ulimit -d); the tree's resident total is watched and kills at the cap")
		case tp.MemoryMB > 0:
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
	// ENG-688: declared egress hosts are enforced under bubblewrap — a fresh
	// network namespace and the forwarder (egress_forwarder_unix.go).
	if ok, why := egressPlan(p, spec, tp); !ok {
		c.egressWhy = why
	} else if bw == "" {
		c.egressWhy = bwWhy
	} else if _, ok := egressOverBwrap(p, spec, tp); !ok {
		c.egressWhy = "the kernel executable (the in-sandbox forwarder) cannot be located"
	} else {
		c.egress = "bwrap"
		c.notes = append(c.notes, "network: own network namespace (loopback only); the kernel's executable, re-run inside as the forwarder, relays the server's proxy port to the kernel's proxy")
	}
	if spec.Agent != nil && tp.DenyNetwork && bw != "" {
		c.notes = append(c.notes, "network: kernel-only (no network namespace; the kernel is reached through the unix socket in the socket directory)")
	}
	if spec.Agent != nil && tp.HideState && runtime.GOOS == "linux" {
		c.notes = append(c.notes, "fs: landlock and seccomp are not applied (bubblewrap mount namespace only)")
	}
	if useSeatbelt(spec, tp) {
		// ADR-0157 S5 (D2.3): the Seatbelt profile (seatbelt.go) enforces the D1
		// shape on macOS, and the network for a kernel-only tier.
		for _, ctl := range []Control{ControlFSConfine, ControlNetwork, ControlHideState} {
			c.enforced[ctl] = MechanismSeatbelt
			delete(c.missing, ctl)
		}
		c.notes = append(c.notes, "fs: Seatbelt (sandbox-exec): root readable, hide_paths denied, writes only to the workdir, the socket directory and declared writable paths",
			"seatbelt: the profile is unit-tested but has not yet run in a live macOS e2e (ADR-0157 S5 follow-up)")
		if tp.DenyNetwork {
			c.notes = append(c.notes, "network: kernel-only (Seatbelt denies the network but the unix sockets in the socket directory)")
		}
	}
	if os.Geteuid() == 0 {
		// A plan that drops to uid 65534 is enforced only when that uid can
		// boot it; when it cannot, the launch is refused before anything starts
		// whatever on_unavailable says (ENG-2026): degrading would mean running
		// the process as root, and starting it anyway only ends in a bwrap
		// stderr line while the report claimed low_privilege.
		why := ""
		if w := traversableByOthers(p.WorkRoot); w != "" {
			why = "uid 65534 could not reach its workdir: " + w +
				" (set mcp.sandbox.work_root outside /root, e.g. /var/lib/cambrian/sandbox)"
		} else if tp.LowPrivilege {
			why = lowUIDBootBlocker(p, spec, useBwrap)
		}
		if why != "" {
			c.missing[ControlLowPrivilege] = why
			if tp.LowPrivilege {
				c.unbootable = why
			}
		} else {
			c.enforced[ControlLowPrivilege] = "uid"
		}
	} else {
		c.missing[ControlLowPrivilege] = "the kernel is not root, so there is no lower uid to switch to; use runner docker"
	}
	return c
}

var seatbeltMemo struct {
	once sync.Once
	path string
}

// seatbeltExec is sandbox-exec's path on macOS ("" elsewhere or when absent).
func seatbeltExec() string {
	seatbeltMemo.once.Do(func() {
		if runtime.GOOS != "darwin" {
			return
		}
		if p, err := exec.LookPath("sandbox-exec"); err == nil {
			seatbeltMemo.path = p
		} else if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
			seatbeltMemo.path = "/usr/bin/sandbox-exec"
		}
	})
	return seatbeltMemo.path
}

// useSeatbelt: an agent on macOS whose tier wants a control the profile gives
// (ADR-0157 S5). MCP servers keep the ulimit-only runner there for now.
func useSeatbelt(spec Spec, tp TierPolicy) bool {
	return spec.Agent != nil && seatbeltExec() != "" &&
		(tp.ConfineFS || tp.HideState || (tp.DenyNetwork && spec.Server.Network != NetworkEgress))
}

// seatbeltInputFor is the profile input for a launch, every path resolved on
// the host (Seatbelt matches resolved paths: /var is /private/var on macOS).
func seatbeltInputFor(p *Policy, spec Spec, tp TierPolicy, workdir string) seatbeltInput {
	canon := func(s string) string {
		if r, err := filepath.EvalSymlinks(s); err == nil {
			return r
		}
		return filepath.Clean(s)
	}
	in := seatbeltInput{Workdir: canon(workdir), HideFiles: map[string]bool{},
		DenyNetwork: tp.DenyNetwork && spec.Server.Network != NetworkEgress}
	if spec.Server.Root != "" {
		in.Root = canon(spec.Server.Root)
	}
	for _, h := range p.HidePaths {
		if h == "" {
			continue
		}
		ch := canon(h)
		in.Hide = append(in.Hide, ch)
		if fi, err := os.Stat(h); err == nil && !fi.IsDir() {
			in.HideFiles[ch] = true
		}
	}
	if spec.Agent != nil {
		if spec.Agent.SocketDir != "" {
			in.SocketDir = canon(spec.Agent.SocketDir)
		}
		for _, b := range spec.Agent.Binds {
			if b.Path == "" {
				continue
			}
			b.Path = canon(b.Path)
			in.Binds = append(in.Binds, b)
		}
	}
	return in
}

func rootNote(spec Spec) string {
	if spec.Server.Root != "" {
		return " and " + spec.Server.Root
	}
	return ""
}

// wantsBwrap: bubblewrap is used only when a control it provides is wanted.
// An enforced egress allowlist wants the network namespace (ENG-688); the
// tiers that want one also confine the filesystem, so it adds no new case
// for the shipped tiers, but a tier with confine_fs off still gets it.
func wantsBwrap(spec Spec, tp TierPolicy) bool {
	return tp.ConfineFS || (tp.DenyNetwork && spec.Server.Network != NetworkEgress) ||
		(tp.DenyNetwork && spec.Server.Network == NetworkEgress && spec.Agent == nil)
}

// egressOverBwrap reports whether this bubblewrap launch enforces its egress
// allowlist, and the forwarder executable to bind into the sandbox.
func egressOverBwrap(p *Policy, spec Spec, tp TierPolicy) (string, bool) {
	if ok, _ := egressPlan(p, spec, tp); !ok {
		return "", false
	}
	exe, err := egressForwarderExe()
	if err != nil {
		return "", false
	}
	return exe, true
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
	if n := tp.taskCeiling(); n > 0 && unprivNS {
		lim = append(lim, "ulimit -u "+strconv.Itoa(n))
	}
	script := strings.Join(append(lim, `exec "$@"`), "; ")
	// ENG-688: an enforced egress allowlist. The server runs as the
	// forwarder's child, inside the sandbox's own network namespace; the
	// forwarder speaks the multiplex on fd 3 to this launch's proxy session.
	var egressSess *egressproxy.Session
	var egressChild *os.File
	serverArgv := append([]string{path}, c.Args...)
	if _, ok := egressOverBwrap(p, spec, tp); ok && useBwrap {
		sess, serr := p.Egress.Open(spec.ServerID, spec.Server.Hosts)
		if serr != nil {
			return fmt.Errorf("sandbox: egress proxy session: %w", serr)
		}
		kc, child, perr := egressPair()
		if perr != nil {
			sess.Close()
			return fmt.Errorf("sandbox: egress multiplex: %w", perr)
		}
		if serr := sess.Serve(kc); serr != nil {
			_ = child.Close()
			sess.Close()
			return serr
		}
		egressSess, egressChild = sess, child
		serverArgv = forwarderArgv(path, c.Args)
		env = append(env, egressproxy.ProxyEnv()...)
	}
	argv := append([]string{"/bin/sh", "-c", script, "sh"}, serverArgv...)
	if useBwrap {
		argv = append(bwrapArgs(bw, p, spec, tp, workdir, c.Dir, unprivNS), argv...)
	}
	if useSeatbelt(spec, tp) {
		// ADR-0157 S5: sandbox-exec applies the profile, then execs the sh
		// wrapper, which applies the limits and execs the agent.
		argv = append([]string{seatbeltExec(), "-p", seatbeltProfile(seatbeltInputFor(p, spec, tp, workdir))}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // operator-configured command
	cmd.Env = env
	cmd.Dir = workdir
	if egressChild != nil {
		cmd.ExtraFiles = []*os.File{egressChild} // fd 3 (egressMuxFD)
	}
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
		if egressSess != nil {
			_ = egressChild.Close()
			egressSess.Close()
		}
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
		return &groupHandle{pid: cmd.Process.Pid, cg: cg, egress: egressSess}, nil
	}
	err = start(cmd, post)
	if egressChild != nil {
		_ = egressChild.Close() // the child holds its own copy once started
	}
	if err != nil && egressSess != nil {
		egressSess.Close()
	}
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
	egressExe, egress := egressOverBwrap(p, spec, tp)
	if egress {
		// ENG-688: the forwarder, inside the private /tmp.
		a = append(a, "--ro-bind", egressExe, egressForwarderPath)
	}
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
	if (tp.DenyNetwork && spec.Server.Network != NetworkEgress) || egress {
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
	// egress is the launch's proxy session (ENG-688), closed with the tree.
	egress *egressproxy.Session
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
		if h.egress != nil {
			h.egress.Close()
		}
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
	if out, err := exec.Command(path, bwrapProbeArgs(os.Geteuid() != 0)...).CombinedOutput(); err != nil { //nolint:gosec // probe of a configured binary
		return "", "bubblewrap is installed but cannot create namespaces here: " + strings.TrimSpace(string(out)) + containerHint()
	}
	return path, ""
}

// bwrapProbeArgs is the usability probe: the mounts and namespaces a launch
// makes (bwrapArgs), so a host where only part of them works — a container
// engine that allows the user namespace but not a fresh /proc — is reported
// missing rather than enforced and then failing at every launch (ENG-2062).
func bwrapProbeArgs(userNS bool) []string {
	a := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-net", "--die-with-parent", "--new-session"}
	if userNS {
		a = append(a, "--unshare-user")
	}
	return append(a, "--", "/bin/true")
}

// containerHint names the fix when the kernel runs in a container, where the
// usual cause is the engine's seccomp/AppArmor policy (ENG-2062).
func containerHint() string {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return " (in a container: allow unprivileged user namespaces, e.g. docker --security-opt seccomp=deploy/seccomp-bwrap.json; see docs/container-sandbox.md)"
		}
	}
	return ""
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

// lowUID is the identity a root kernel drops a low_privilege process to.
const lowUID = 65534

// lowUIDBootBlocker reports why a process dropped to uid 65534 could not boot
// this launch ("" if it can), so the plan refuses it naming the path and the
// fix instead of starting it into a bwrap error (ENG-2026).
//
// Under bubblewrap the sandbox runs as uid 65534 in its own user namespace,
// where it has no rights over root-owned directories. To mount a bind (or a
// hidden path's tmpfs or /dev/null) at a host path, bwrap re-creates the path's
// parents under its new root, and that fails ("Can't mkdir parents …:
// Permission denied") when any parent is not both readable and searchable by
// uid 65534 — a root-owned 0711 directory included — whatever the mounted
// path's own owner or mode (measured with bubblewrap 0.12.0). A parent inside
// a hidden directory is exempt: the hidden directory is a fresh tmpfs, so the
// mount point is created there. Without bubblewrap nothing is mounted and the
// process only has to walk to each path: every parent needs the search bit.
func lowUIDBootBlocker(p *Policy, spec Spec, useBwrap bool) string {
	var hiddenDirs, mounts []string
	for _, h := range p.HidePaths {
		if h == "" {
			continue
		}
		if fi, err := os.Stat(h); err == nil {
			if fi.IsDir() {
				hiddenDirs = append(hiddenDirs, h)
			}
			if useBwrap {
				mounts = append(mounts, h)
			}
		}
	}
	var binds []string
	if spec.Agent != nil {
		kept, _ := privateTmpSafe(spec.Agent.Binds)
		for _, b := range kept {
			binds = append(binds, b.Path)
		}
		if spec.Agent.SocketDir != "" {
			binds = append(binds, spec.Agent.SocketDir)
		}
	}
	if spec.Server.Root != "" {
		binds = append(binds, spec.Server.Root)
	}
	need, verb, fix := os.FileMode(0o5), "read and search", "o+rx"
	if !useBwrap {
		need, verb, fix = 0o1, "search", "o+x"
	}
	for _, m := range append(mounts, binds...) {
		if m == "" {
			continue
		}
		if _, err := os.Stat(m); err != nil {
			continue // a missing bind is dropped, not mounted (splitBinds)
		}
		m = filepath.Clean(m)
		for d := filepath.Dir(m); ; d = filepath.Dir(d) {
			inHidden := false
			for _, h := range hiddenDirs {
				if within(d, h) {
					inHidden = true
					break
				}
			}
			if fi, err := os.Stat(d); err == nil && !inHidden && !lowUIDMay(fi, need) {
				return fmt.Sprintf("uid 65534 cannot boot this launch: %s is under %s (mode %04o), which uid 65534 cannot %s, "+
					"so the sandbox cannot set up %s; give %s %s (e.g. chmod 0755 %s) or move %s elsewhere",
					m, d, fi.Mode().Perm(), verb, m, d, fix, d, m)
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return ""
}

// lowUIDMay reports whether uid 65534 (no supplementary groups) has the need
// bits (r=4, w=2, x=1) on fi under DAC: the owner bits if it owns fi, the group
// bits if fi's group is 65534, else the other bits.
func lowUIDMay(fi os.FileInfo, need os.FileMode) bool {
	perm, shift := fi.Mode().Perm(), 0
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		switch {
		case st.Uid == lowUID:
			shift = 6
		case st.Gid == lowUID:
			shift = 3
		}
	}
	return (perm>>shift)&need == need
}
