//go:build windows

package sandbox

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// Windows native containment:
//
//   - Job Object (every tier): a job memory ceiling, an active-process ceiling,
//     a per-job CPU-time ceiling, KILL_ON_JOB_CLOSE so the whole tree dies with
//     the job, and (when confined) UI restrictions (clipboard, desktop, global
//     atoms, other processes' USER handles). The process is created SUSPENDED,
//     assigned, then resumed, so not one instruction runs outside the job.
//   - Low-integrity token (fs_confine, low_privilege): the process runs with
//     the kernel's own token duplicated at LOW integrity. Mandatory integrity
//     control's no-write-up then denies writes to every medium-integrity
//     object: the user profile, the repository, the kernel's config and its
//     process. The server's workdir is labelled low so it is the one place it
//     can write. READS are not confined (no-read-up is not the default label
//     policy), and the identity is the same user SID at a lower integrity.
//   - AppContainer (agents, ADR-0157 S5; appcontainer_windows.go): an agent
//     whose tier wants hidden state or a denied network runs under a lowbox
//     token instead of the plain low-integrity one. It reads system
//     directories and the paths the kernel granted it, nothing else — the D1
//     hide-list is unreadable, checked by AccessCheck — and has no network
//     unless its tier keeps one. When a grant is impossible (an interpreter
//     owned by Administrators) the agent falls back to the low-integrity token
//     and the report names hide_state and network missing, with the reason.
//   - AppContainer behind the egress proxy (MCP servers whose declared hosts
//     are enforced, ENG-2215; appcontainer_mcp_windows.go): no network
//     capability, the kernel's executable re-run inside as the forwarder
//     (egress_forwarder_windows.go) relays the server's 127.0.0.1 proxy port
//     to the kernel's CONNECT proxy over an AF_UNIX socket in the workdir.
//     When the server's runtime cannot be granted to the container the server
//     keeps the low-integrity token and its hosts are recorded, not filtered.
//   - NOT available natively for other MCP servers: a network deny. They keep
//     the low-integrity token (T-20261106-windows-appcontainer-runner-for-
//     native-network); Windows Filtering Platform needs admin. A third-party
//     server that must have no network uses runner docker.
func nativeCapability(p *Policy, spec Spec, tp TierPolicy, cmd Command) capability {
	c := capability{
		runner: RunnerNative,
		enforced: map[Control]string{
			ControlEnv: "", ControlWorkdir: "", ControlOutputCap: "", ControlStallKill: "",
			ControlKillTree: "job_object", ControlMemory: "job_object",
			ControlProcesses: "job_object", ControlCPUTime: "job_object",
			ControlFSConfine: "low_integrity", ControlLowPrivilege: "low_integrity",
		},
		missing: map[Control]string{
			ControlNetwork: "no native egress control on Windows (AppContainer/WFP not built); declare egress or use runner docker",
			// ADR-0157 D2.4: a low-integrity token denies writes up, never reads.
			ControlHideState: "Windows low integrity confines writes only: reads of configs/, the key file and the data directories are NOT confined (the AppContainer-class runner is ADR-0157 S5)",
		},
	}
	if spec.Agent != nil {
		// ADR-0157 D3: a kernel-only agent tier needs a native network control
		// Windows does not have yet; the tier's on_unavailable decides.
		c.missing[ControlNetwork] = "no native network control on Windows for agents: the kernel-only agent network needs the AppContainer runner (ADR-0157 S5)"
	}
	// ENG-688/ENG-2215: declared egress hosts are enforced by running the
	// server in an AppContainer without network capabilities behind the
	// kernel's proxy, when its runtime can be granted to the container.
	if ok, why := egressPlan(p, spec, tp); !ok {
		c.egressWhy = why
	} else if !wantsMCPEgressContainer(p, spec, tp) {
		c.egressWhy = "the tier runs the server unconfined (fs_confine and low_privilege off), outside any AppContainer"
	} else if ac, _, why := mcpEgressContainer(p, spec, cmd); ac == nil {
		c.egressWhy = why
	} else {
		ac.close()
		c.egress = MechanismAppContainer
		c.enforced[ControlFSConfine] = MechanismAppContainer
		c.enforced[ControlLowPrivilege] = MechanismAppContainer
		c.notes = append(c.notes, "network: an AppContainer with no network capability; the kernel's executable, re-run inside as the forwarder, relays the server's proxy port to the kernel's proxy over a unix socket in the workdir",
			"fs: AppContainer: the server reads system directories, its command's installation and the forwarder only; writes only to the workdir")
		return c
	}
	if wantsAppContainer(spec, tp) {
		ac, pl := appContainerFor(p, spec, tp, true)
		ac.close()
		if applyAppContainer(&c, tp, pl) {
			return c
		}
	}
	if tp.ConfineFS {
		c.notes = append(c.notes, "fs: writes confined to the workdir by integrity level; reads are not confined")
		if spec.Server.Root != "" {
			c.notes = append(c.notes, "fs: declared root "+spec.Server.Root+" stays read-only (only the workdir is labelled low-integrity)")
		}
	}
	return c
}

// applyAppContainer folds what the AppContainer can deliver into c. It returns
// true when the agent will run in it; false when the runner falls back to the
// low-integrity token (the reason is noted and named in the missing controls).
func applyAppContainer(c *capability, tp TierPolicy, pl acPlan) bool {
	c.notes = append(c.notes, pl.notes...)
	if !pl.ok {
		c.notes = append(c.notes, "appcontainer: unusable, fell back to low integrity: "+pl.why)
		c.missing[ControlHideState] += "; the AppContainer runner is unusable here: " + pl.why
		return false
	}
	c.enforced[ControlFSConfine] = MechanismAppContainer
	c.enforced[ControlLowPrivilege] = MechanismAppContainer
	c.enforced[ControlNetwork] = MechanismAppContainer
	delete(c.missing, ControlNetwork)
	if len(pl.readable) == 0 {
		c.enforced[ControlHideState] = MechanismAppContainer
		delete(c.missing, ControlHideState)
	} else {
		c.missing[ControlHideState] = "the agent container can still read " + strings.Join(pl.readable, ", ") +
			" (an ancestor grants ALL APPLICATION PACKAGES or a cambrian capability; move the kernel's state out of it)"
	}
	c.notes = append(c.notes, "fs: AppContainer: the agent reads system directories and its granted paths only "+
		"(stricter than the Linux read-only root); hide_paths checked unreadable by AccessCheck; writes only to the workdir, socket directory and declared writable paths")
	if tp.DenyNetwork {
		c.notes = append(c.notes, "network: kernel-only (an AppContainer without network capabilities; the kernel is reached through the unix socket in the socket directory)")
	} else {
		c.notes = append(c.notes, "network: internet and private-network capabilities; loopback is blocked for every AppContainer, so the kernel is reached through the unix socket in the socket directory")
	}
	return true
}

func startNative(p *Policy, spec Spec, tp TierPolicy, c Command, workdir string, start starter) error {
	lowIL := tp.ConfineFS || tp.LowPrivilege
	env, err := workdirEnv(p, c.Env, tp.ConfineFS, workdir, workdir, `\`)
	if err != nil {
		return err
	}
	// ADR-0157 S5: the agent's AppContainer, when its tier wants one and the
	// grants succeed (the plan made the same decision and reported it).
	var ac *appContainer
	if lowIL && wantsAppContainer(spec, tp) {
		var pl acPlan
		if ac, pl = appContainerFor(p, spec, tp, false); !pl.ok {
			ac = nil
		}
	}
	defer ac.close() // the process holds its own copy of the token once created
	// ENG-2215: an egress-enforced MCP server runs in its own AppContainer
	// (no network capability) as the forwarder's child. The plan made the
	// same decision; a container that cannot be prepared now leaves the
	// server on the low-integrity token, as the plan would have reported.
	var mcpAC *appContainer
	serverPath, serverArgs := c.Path, c.Args
	if lowIL && wantsMCPEgressContainer(p, spec, tp) {
		var why string
		if mcpAC, serverPath, why = mcpEgressContainer(p, spec, c); mcpAC == nil {
			serverPath = c.Path
			slog.Warn("sandbox: egress container unavailable; declared hosts recorded, not filtered", "server", spec.ServerID, "why", why)
		}
	}
	defer mcpAC.close()
	if lowIL {
		for _, dir := range lowWritable(workdir, spec) {
			// ENG-2272: a launch on the plain low-integrity token (no
			// container this time) must not inherit an earlier launch's
			// AppContainer grant on the same workdir. With any AppContainer
			// SID ACE in the workdir's DACL the low-integrity process was
			// denied the directory itself (found live: uvx "Failed to
			// initialize cache … os error 183" on a workdir an egress-container
			// launch had granted), so the stale grant is revoked first; the
			// next container launch grants it again.
			if ac == nil && mcpAC == nil {
				if err := revokeAppContainerACEs(dir); err != nil {
					return fmt.Errorf("sandbox: revoke stale AppContainer grants on %s: %w", dir, err)
				}
			}
			if err := labelLowIntegrity(dir); err != nil {
				return fmt.Errorf("sandbox: label %s low-integrity: %w", dir, err)
			}
		}
	}
	if ac != nil {
		for _, dir := range instanceDirs(workdir, spec) {
			if err := ac.grantInstanceDir(dir); err != nil {
				return fmt.Errorf("sandbox: grant the agent container %s: %w", dir, err)
			}
		}
	}
	var egress *egressLink
	if mcpAC != nil {
		if err := mcpAC.grantInstanceDir(workdir); err != nil {
			return fmt.Errorf("sandbox: grant the server container %s: %w", workdir, err)
		}
		if egress, err = openEgressLink(p, spec, workdir); err != nil {
			return err
		}
		exe, _ := egressForwarderExe() // located by mcpEgressContainer
		argv := forwarderArgv(exe, egress.socket, serverPath, c.Args)
		serverPath, serverArgs = argv[0], argv[1:]
		env = append(env, egressproxy.ProxyEnv()...)
	}
	job, err := newJob(tp, tp.ConfineFS)
	if err != nil {
		egress.close()
		return fmt.Errorf("sandbox: job object: %w", err)
	}
	cmd := exec.Command(serverPath, serverArgs...) //nolint:gosec // operator-configured command (or the forwarder wrapping it)
	cmd.Env = env
	cmd.Dir = workdir
	if c.Dir != "" {
		cmd.Dir = c.Dir
	}
	attr := &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	var tok windows.Token
	switch {
	case ac != nil:
		attr.Token = syscall.Token(ac.token)
	case mcpAC != nil:
		attr.Token = syscall.Token(mcpAC.token)
	case lowIL:
		tok, err = lowIntegrityToken()
		if err != nil {
			_ = windows.CloseHandle(job)
			egress.close()
			return fmt.Errorf("sandbox: low-integrity token: %w", err)
		}
		attr.Token = syscall.Token(tok)
	}
	cmd.SysProcAttr = attr
	// Ownership: the job and the token stay with this function until post
	// succeeds; then the job belongs to the jobHandle. Every failure path
	// closes each handle exactly once (a double CloseHandle could close a
	// handle the OS has since reused).
	post := func() (osHandle, error) {
		ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			return nil, fmt.Errorf("sandbox: open process: %w", err)
		}
		defer func() { _ = windows.CloseHandle(ph) }()
		if err := windows.AssignProcessToJobObject(job, ph); err != nil {
			return nil, fmt.Errorf("sandbox: assign to job: %w", err)
		}
		h, err := newJobHandle(job, tp, spec.Agent != nil)
		if err != nil {
			_ = windows.TerminateJobObject(job, 1)
			return nil, fmt.Errorf("sandbox: job limit watcher: %w", err)
		}
		if err := resumeProcess(uint32(cmd.Process.Pid)); err != nil {
			_ = windows.TerminateJobObject(job, 1)
			h.stopWatcher()
			return nil, fmt.Errorf("sandbox: resume: %w", err)
		}
		if egress != nil {
			return &egressJobHandle{jobHandle: h, egress: egress}, nil
		}
		return h, nil
	}
	err = start(cmd, post)
	if tok != 0 {
		_ = tok.Close() // the process holds its own copy once created
	}
	if err != nil {
		_ = windows.CloseHandle(job)
		egress.close()
	}
	return err
}

// egressLink is an egress-enforced launch's proxy session and the AF_UNIX
// listener its forwarder dials (ENG-2215). One connection is accepted and
// served; the listener then closes. close ends both and is idempotent.
type egressLink struct {
	socket string
	sess   *egressproxy.Session
	ln     net.Listener
	done   chan struct{}
	once   sync.Once
}

// openEgressLink opens the launch's proxy session and listens on the
// multiplex socket in the (already granted, low-integrity) workdir.
func openEgressLink(p *Policy, spec Spec, workdir string) (*egressLink, error) {
	sock, err := egressSocketPath(workdir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	_ = os.Remove(sock) // a previous launch's socket file in a reused workdir
	sess, err := p.Egress.Open(spec.ServerID, spec.Server.Hosts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: egress proxy session: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sandbox: egress socket: %w", err)
	}
	l := &egressLink{socket: sock, sess: sess, ln: ln, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		conn, aerr := ln.Accept()
		_ = ln.Close()
		if aerr != nil {
			return // closed before the forwarder dialled (a failed start, or close)
		}
		if serr := sess.Serve(conn); serr != nil {
			slog.Warn("sandbox: egress proxy session refused the forwarder", "server", spec.ServerID, "err", serr)
		}
	}()
	return l, nil
}

func (l *egressLink) close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		_ = l.ln.Close()
		<-l.done
		l.sess.Close()
		_ = os.Remove(l.socket)
	})
}

// egressJobHandle is the job handle of an egress-enforced launch: releasing
// it also ends the proxy session.
type egressJobHandle struct {
	*jobHandle
	egress *egressLink
}

func (h *egressJobHandle) release() {
	h.jobHandle.release()
	h.egress.close()
}

// instanceDirs are an agent instance's own directories: its workdir (scratch)
// and its socket directory.
func instanceDirs(workdir string, spec Spec) []string {
	dirs := []string{workdir}
	if spec.Agent != nil && spec.Agent.SocketDir != "" {
		dirs = append(dirs, spec.Agent.SocketDir)
	}
	return dirs
}

// lowWritable lists the directories a low-integrity process must be able to
// write: its workdir and, for an agent (ADR-0157 D1/D2.6), its socket directory
// and every path its manifest declared writable and the operator admitted.
func lowWritable(workdir string, spec Spec) []string {
	dirs := []string{workdir}
	if spec.Agent == nil {
		return dirs
	}
	if spec.Agent.SocketDir != "" {
		dirs = append(dirs, spec.Agent.SocketDir)
	}
	for _, b := range spec.Agent.Binds {
		if b.Write {
			dirs = append(dirs, b.Path)
		}
	}
	return dirs
}

// jobHandle owns the job and its limit watcher. kill terminates every process
// in it; release stops the watcher and closes the handles (KILL_ON_JOB_CLOSE
// makes that a kill too).
//
// The watcher exists because a job MEMORY limit only makes allocations fail:
// a runtime that retries or hangs in its out-of-memory path (the Go runtime
// does) sits at the ceiling forever. The job's completion port reports
// JOB_OBJECT_MSG_JOB_MEMORY_LIMIT, and the watcher terminates the job on it.
type jobHandle struct {
	mu     sync.Mutex
	job    windows.Handle
	port   windows.Handle
	done   chan struct{}
	reason string
}

// Job completion-port messages (winnt.h).
const (
	jobMsgEndOfJobTime       = 1
	jobMsgActiveProcessLimit = 3
	jobMsgJobMemoryLimit     = 10
	jobMsgStop               = 0xC0DE // our own: stop the watcher
)

type jobAssocPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// newJobHandle starts the job's limit watcher. killOnProcessLimit (agent
// launches, ENG-2112): reaching the active-process limit kills the tree, so a
// fork-bombing agent dies into the restart policy instead of living on at the
// ceiling with every CreateProcess failing; an MCP server keeps the old
// behaviour (the spawn fails, the server lives).
func newJobHandle(job windows.Handle, tp TierPolicy, killOnProcessLimit bool) (*jobHandle, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, err
	}
	assoc := jobAssocPort{CompletionKey: 1, CompletionPort: port}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		_ = windows.CloseHandle(port)
		return nil, err
	}
	h := &jobHandle{job: job, port: port, done: make(chan struct{})}
	go h.watch(tp, killOnProcessLimit)
	return h, nil
}

func (h *jobHandle) watch(tp TierPolicy, killOnProcessLimit bool) {
	defer close(h.done)
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(h.port, &msg, &key, &ov, windows.INFINITE); err != nil {
			return
		}
		switch msg {
		case jobMsgStop:
			return
		case jobMsgJobMemoryLimit:
			h.setReason(fmt.Sprintf("memory: the process tree reached its %d MiB limit", tp.MemoryMB))
			_ = h.kill()
		case jobMsgActiveProcessLimit:
			if killOnProcessLimit {
				h.setReason(fmt.Sprintf("processes: the process tree reached its %d-process limit", tp.MaxProcesses))
				_ = h.kill()
			}
		case jobMsgEndOfJobTime:
			h.setReason(fmt.Sprintf("cpu_time: the process tree used its %d s of CPU", tp.CPUTimeSec))
		}
	}
}

func (h *jobHandle) setReason(r string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		h.reason = r
	}
}

func (h *jobHandle) limitReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

func (h *jobHandle) kill() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(h.job, 1)
}

// stopWatcher stops the watcher and closes the port but leaves the job
// handle to its owner (a failed start).
func (h *jobHandle) stopWatcher() {
	_ = windows.PostQueuedCompletionStatus(h.port, jobMsgStop, 0, nil)
	<-h.done
	_ = windows.CloseHandle(h.port)
	h.port = 0
}

func (h *jobHandle) release() {
	h.mu.Lock()
	port := h.port
	h.mu.Unlock()
	if port != 0 {
		_ = windows.PostQueuedCompletionStatus(port, jobMsgStop, 0, nil)
		<-h.done
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.job != 0 {
		_ = windows.CloseHandle(h.job)
		h.job = 0
	}
	if h.port != 0 {
		_ = windows.CloseHandle(h.port)
		h.port = 0
	}
}

func newJob(tp TierPolicy, uiRestrict bool) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	if tp.MemoryMB > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(tp.MemoryMB) << 20
	}
	if tp.MaxProcesses > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = uint32(tp.MaxProcesses)
	}
	if tp.CPUTimeSec > 0 {
		// PerJobUserTimeLimit is in 100 ns units; exceeding it terminates every
		// process in the job.
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_TIME
		info.BasicLimitInformation.PerJobUserTimeLimit = int64(tp.CPUTimeSec) * 10_000_000
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	if uiRestrict {
		ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_DESKTOP |
			windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS | windows.JOB_OBJECT_UILIMIT_EXITWINDOWS |
			windows.JOB_OBJECT_UILIMIT_GLOBALATOMS | windows.JOB_OBJECT_UILIMIT_HANDLES |
			windows.JOB_OBJECT_UILIMIT_READCLIPBOARD | windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
			windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD}
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectBasicUIRestrictions,
			uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
			_ = windows.CloseHandle(job)
			return 0, err
		}
	}
	return job, nil
}

// resumeProcess resumes every thread of a process created suspended. exec.Cmd
// does not expose the primary thread handle, so the threads are found through
// a toolhelp snapshot; a fresh suspended process has exactly one.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return oerr
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("no thread found for pid %d", pid)
	}
	return nil
}

// lowIntegrityToken duplicates the kernel's primary token at LOW integrity.
// Lowering one's own token's integrity needs no privilege, and a restricted
// copy of the caller's own token is assignable to CreateProcessAsUser.
func lowIntegrityToken() (windows.Token, error) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY, &cur); err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close() }()
	var dup windows.Token
	if err := windows.DuplicateTokenEx(cur, 0, nil, windows.SecurityImpersonation, windows.TokenPrimary, &dup); err != nil {
		return 0, err
	}
	sid, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		_ = dup.Close()
		return 0, err
	}
	tml := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(dup, windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&tml)), tml.Size()); err != nil {
		_ = dup.Close()
		return 0, err
	}
	return dup, nil
}

// labelLowIntegrity gives a directory (and, by inheritance, everything created
// in it) a LOW mandatory label, so a low-integrity process may write there.
func labelLowIntegrity(dir string) error {
	sd, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;LW)")
	if err != nil {
		return err
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION,
		nil, nil, nil, sacl)
}
