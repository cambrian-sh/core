package agentmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 S1 — agents launch through internal/sandbox with D1 confinement on
// every tier (execution.agents.sandbox.enabled). The instance manager keeps its
// exec.Cmd semantics: sandbox.StartCmd hands back the started command, the
// manager reads its pipes, Waits on it once (watchExit) and releases the
// sandbox when that Wait returns.
//
// Per instance the agent gets:
//   - a private scratch directory, <work_root>/<tier>/<agent>-<instance>, its
//     TEMP/TMP/TMPDIR/HOME, removed when the process exits (D1.2);
//   - a socket directory, <work_root>/sock/<instance>, shown at its own path
//     (D2.6: --socket and --substrate-addr are unchanged);
//   - read-only: its code directory, its interpreter's tree (for Python the
//     kernel-managed env's current generation, ADR-0157 D7), the
//     kernel-known runtime paths (agents/, prompts/, skills/) and the kernel's
//     TLS CA file when it has one (ADR-0130 D4: a public certificate);
//   - the paths its manifest declares (sandbox_paths), each admitted by the
//     operator's allowlist_cap and never inside the kernel's own state.
//
// Everything in the D1 hide-list is unreadable where the runner can do it, and
// the boot log says per agent which controls are enforced and which are missing.

// AgentSandbox is the resolved agent sandbox (execution.agents.sandbox with
// enabled: true), built once at the composition root by NewAgentSandbox.
type AgentSandbox struct {
	policy       *sandbox.Policy
	dockerImage  string
	dockerUser   string
	allowlistCap []string
	kernelState  []string
	runtimeRead  []string
	modelCache   string

	// owner is this kernel's claim on the work root (agent_workroot.go,
	// ENG-2088): set by OwnWorkRoot at boot; nil ⇒ launches claim nothing.
	ownerMu sync.Mutex
	owner   *workRootOwner
}

// AgentSandboxOptions is what NewAgentSandbox needs from config.
type AgentSandboxOptions struct {
	Runner                  string // native | docker
	WorkRoot                string // "" ⇒ <user cache>/cambrian/agents
	FirstPartyRefuse        bool   // first_party.on_unavailable == refuse
	ThirdPartyRefuse        bool   // third_party.on_unavailable != degrade
	HidePaths               []string
	KernelStatePaths        []string
	KernelDir               string
	RuntimeReadPaths        []string
	AllowlistCap            []string
	DockerImage, DockerUser string
	BwrapPath, DockerPath   string
	CgroupParent            string
	// KernelOnlyAll is execution.agents.sandbox.network: kernel_only_all —
	// every tier kernel-only, not only third-party (ADR-0157 D3).
	KernelOnlyAll bool
	// ModelCache is the agents' shared model cache (ADR-0157 S7,
	// execution.agents.model_cache_dir, resolved by the composition root): shown
	// to every instance at its own path, writable for first-party and system
	// agents, read-only for third-party. "" ⇒ none.
	ModelCache string
	// FirstPartyLimits / ThirdPartyLimits are each tier's process, task and
	// scratch limits (execution.agents.sandbox.<tier>.max_processes,
	// max_tasks, scratch_mb; ENG-2112). The memory cap travels per launch
	// (admitLaunch, then sandbox.ServerSpec.MemoryMB) because a manifest may
	// lower it.
	FirstPartyLimits, ThirdPartyLimits AgentTierLimits
}

// AgentTierLimits is one agent tier's resource limits (ENG-2112). Exceeding
// one kills the instance (the internal/sandbox limit watch, the Windows job).
// 0 leaves that limit off; the composition root always passes the config's
// defaulted values.
type AgentTierLimits struct {
	MaxProcesses int
	MaxTasks     int
	ScratchMB    int
}

// NewAgentSandbox builds the agent tiers' sandbox policy.
//
// Tiers (D1, D3): first-party (system agents run at it too) wants the env
// allowlist, a workdir, a killable tree, the memory cap, the process and
// scratch limits (ENG-2112), write confinement and hidden kernel state — and, when the kernel runs as root on Unix, a separate
// uid (D2.2). Third-party additionally always wants a low-privilege identity
// and the network denied: it is KERNEL-ONLY (ADR-0157 S2), reaching the kernel
// through a unix socket in its own socket directory (kernelSocketPath) and
// nothing else. First-party keeps the normal network unless KernelOnlyAll
// (execution.agents.sandbox.network: kernel_only_all) makes it kernel-only too.
func NewAgentSandbox(o AgentSandboxOptions) *AgentSandbox {
	runner := o.Runner
	if runner == "" {
		runner = sandbox.RunnerNative
	}
	workRoot := o.WorkRoot
	if workRoot == "" {
		workRoot = defaultAgentWorkRoot()
	}
	onUnavail := func(refuse bool) string {
		if refuse {
			return sandbox.OnUnavailableRefuse
		}
		return sandbox.OnUnavailableDegrade
	}
	rootUnix := goruntime.GOOS != "windows" && os.Geteuid() == 0
	hide := dedupePaths(append(append(append([]string{o.KernelDir}, o.KernelStatePaths...), o.HidePaths...), workRoot))
	p := &sandbox.Policy{
		DefaultTier: sandbox.TierThirdParty,
		Tiers: map[sandbox.Tier]sandbox.TierPolicy{
			sandbox.TierFirstParty: {OnUnavailable: onUnavail(o.FirstPartyRefuse), Runner: runner,
				ConfineFS: true, HideState: true, LowPrivilege: rootUnix, DenyNetwork: o.KernelOnlyAll,
				MaxProcesses: o.FirstPartyLimits.MaxProcesses, MaxTasks: o.FirstPartyLimits.MaxTasks,
				ScratchMB: o.FirstPartyLimits.ScratchMB},
			sandbox.TierThirdParty: {OnUnavailable: onUnavail(o.ThirdPartyRefuse), Runner: runner,
				ConfineFS: true, HideState: true, LowPrivilege: true, DenyNetwork: true,
				MaxProcesses: o.ThirdPartyLimits.MaxProcesses, MaxTasks: o.ThirdPartyLimits.MaxTasks,
				ScratchMB: o.ThirdPartyLimits.ScratchMB},
		},
		WorkRoot:     workRoot,
		WorkdirEnv:   agentWorkdirEnv(goruntime.GOOS),
		BwrapPath:    o.BwrapPath,
		DockerPath:   o.DockerPath,
		CgroupParent: o.CgroupParent,
		HidePaths:    hide,
	}
	state := dedupePaths(append(append([]string{}, o.KernelStatePaths...), o.HidePaths...))
	state = append(state, workRoot)
	return &AgentSandbox{
		policy: p, dockerImage: o.DockerImage, dockerUser: o.DockerUser,
		allowlistCap: expandAll(o.AllowlistCap), kernelState: state,
		runtimeRead: dedupePaths(o.RuntimeReadPaths),
		modelCache:  o.ModelCache,
	}
}

// modelCacheBind is the shared model cache as this agent is shown it (ADR-0157
// S7): writable for first-party and system agents, whose organs download and
// reuse weights there; read-only for third-party agents, which may load what is
// cached but never plant weights a first-party organ would later load. Private,
// so the Windows AppContainer grants it to the agent's own capability with the
// tier's access, never to the runtime capability every tier holds.
func (s *AgentSandbox) modelCacheBind(def *domain.AgentDefinition) (sandbox.Bind, bool) {
	if s.modelCache == "" {
		return sandbox.Bind{}, false
	}
	return sandbox.Bind{Path: filepath.Clean(s.modelCache), Write: agentTrustTier(def) != agentTierThirdParty, Private: true}, true
}

// agentWorkdirEnv is the environment every sandboxed agent instance gets
// pointed into its private scratch directory: TEMP/TMP/TMPDIR and HOME, and
// CAMBRIAN_AGENT_OUT, the instance's out/ folder (ADR-0157 D5.1) whose files
// the kernel collects into the run ledger when it exits.
//
// On Windows the user profile is named by USERPROFILE, APPDATA and
// LOCALAPPDATA, not HOME: Python's Path.home() / os.path.expanduser read
// USERPROFILE, and the cache libraries (Hugging Face, torch, docling) build
// their default paths from it. Left at the kernel user's profile, the first
// stat of it fails inside the AppContainer, which is allowlist-only (found by
// the ADR-0157 S7 live run: the SDK's host_facts() raised PermissionError on
// C:\Users\<user> and failed every chat turn). So the profile moves into the
// scratch too, as HOME does on Unix.
func agentWorkdirEnv(goos string) map[string]string {
	env := map[string]string{"TEMP": "tmp", "TMP": "tmp", "TMPDIR": "tmp", "HOME": "home",
		AgentOutEnv: AgentOutDir}
	if goos == "windows" {
		env["USERPROFILE"] = "home"
		env["APPDATA"] = "home/AppData/Roaming"
		env["LOCALAPPDATA"] = "home/AppData/Local"
	}
	return env
}

// Policy is the agent sandbox policy (tests, status surfaces).
func (s *AgentSandbox) Policy() *sandbox.Policy { return s.policy }

func defaultAgentWorkRoot() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "cambrian", "agents")
	}
	return filepath.Join(os.TempDir(), "cambrian-agents")
}

// sandboxTier maps the kernel's agent tier to a launch tier. A system agent is
// first-party for containment: ADR-0157 D1 holds for system agents too, and
// "system" is never a launch tier (ADR-0139 D8).
func sandboxTier(def *domain.AgentDefinition) sandbox.Tier {
	if agentTrustTier(def) == agentTierThirdParty {
		return sandbox.TierThirdParty
	}
	return sandbox.TierFirstParty
}

// shortInstance is the 8-hex form of an instance id used in paths.
func shortInstance(instanceID string) string {
	s := strings.ReplaceAll(instanceID, "-", "")
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

// socketDir is an instance's socket directory under the sandbox work root.
func (s *AgentSandbox) socketDir(instanceID string) string {
	return filepath.Join(s.policy.WorkRoot, "sock", shortInstance(instanceID))
}

// kernelSocketName is the kernel's agent-plane socket in a kernel-only
// instance's socket directory (ADR-0157 S2). It cannot collide with the
// agent's own socket, whose name starts with "cambrian_" (socketPathIn).
const kernelSocketName = "kernel.sock"

// kernelSocketPath is a kernel-only instance's kernel socket.
func (s *AgentSandbox) kernelSocketPath(instanceID string) string {
	return filepath.Join(s.socketDir(instanceID), kernelSocketName)
}

// kernelOnly reports whether an agent of this tier is kernel-only by policy on
// a Unix host (ADR-0157 D3): its tier denies the network, and bubblewrap,
// docker or Seatbelt pass the unix kernel socket into the sandbox.
func (s *AgentSandbox) kernelOnly(t sandbox.Tier) bool {
	return s.policy.Tiers[t].DenyNetwork && goruntime.GOOS != "windows"
}

// kernelSocketTransport reports whether this instance reaches the kernel
// through its kernel socket instead of TCP. Unix: when its tier is kernel-only
// (ADR-0157 S2). Windows: when it runs in the AppContainer (S5), whatever its
// tier, because an AppContainer cannot reach loopback at all; an agent that
// falls back to the low-integrity token keeps TCP, and its report names the
// network missing.
func (s *AgentSandbox) kernelSocketTransport(ctx context.Context, spec sandbox.Spec, c sandbox.Command) bool {
	if goruntime.GOOS != "windows" {
		return s.kernelOnly(spec.Tier)
	}
	return slices.Contains(sandbox.Plan(ctx, spec, c).Mechanisms, sandbox.MechanismAppContainer)
}

// KernelOnlyTiers names the tiers whose agents are kernel-only (boot log).
func (s *AgentSandbox) KernelOnlyTiers() []string {
	var out []string
	for _, t := range []sandbox.Tier{sandbox.TierFirstParty, sandbox.TierThirdParty} {
		if s.policy.Tiers[t].DenyNetwork {
			out = append(out, string(t))
		}
	}
	return out
}

// kernelOnlyCommand points a kernel-only instance at its kernel socket: the
// value of --substrate-addr becomes unix:<path> (the flag and its place in argv
// are unchanged, D2.6), and over TLS the server name the agent verifies is the
// one the certificate gives for the socket's "localhost" authority.
func (im *InstanceManager) kernelOnlyCommand(c sandbox.Command, kernelSock string) sandbox.Command {
	args := append([]string(nil), c.Args...)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--substrate-addr" {
			args[i+1] = "unix:" + kernelSock
			break
		}
	}
	c.Args = args
	if im.agentTLSCA != "" {
		env := make([]string, 0, len(c.Env)+1)
		for _, kv := range c.Env {
			if !strings.HasPrefix(kv, TLSServerNameEnv+"=") {
				env = append(env, kv)
			}
		}
		if im.agentUnixTLSServerName != "" {
			env = append(env, TLSServerNameEnv+"="+im.agentUnixTLSServerName)
		}
		c.Env = env
		c.Forward = forwardNames(env)
	}
	return c
}

// agentSocketPath is where an instance serves its socket: the OS temp
// directory (unchanged) without the sandbox, its socket directory with it.
func (im *InstanceManager) agentSocketPath(agentID, instanceID string) string {
	if sb := im.containment.Sandbox; sb != nil {
		return socketPathIn(sb.socketDir(instanceID), agentID, instanceID)
	}
	return socketPath(agentID, instanceID)
}

// ErrSandboxPathRefused is wrapped by a boot refused over a declared path.
var ErrSandboxPathRefused = errors.New("sandbox path refused")

// admitDeclared checks an agent's manifest sandbox_paths against the
// operator's cap and the kernel's state (ADR-0157 D1.2): refused with the path
// named, never silently dropped.
func (s *AgentSandbox) admitDeclared(agentID string, decl []domain.SandboxPath) ([]sandbox.Bind, error) {
	var out []sandbox.Bind
	for _, d := range decl {
		p := expandHome(d.Path)
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("agent %s: sandbox_paths '%s' is not an absolute path: %w", agentID, d.Path, ErrSandboxPathRefused)
		}
		p = filepath.Clean(p)
		for _, st := range s.kernelState {
			if withinPath(p, st) {
				return nil, fmt.Errorf("agent %s: sandbox_paths '%s' lies inside the kernel's own state (%s), which no agent may see (ADR-0157 D1): %w",
					agentID, d.Path, st, ErrSandboxPathRefused)
			}
		}
		admitted := false
		for _, c := range s.allowlistCap {
			if withinPath(p, c) {
				admitted = true
				break
			}
		}
		if !admitted {
			return nil, fmt.Errorf("agent %s declares sandbox path '%s', which execution.agents.sandbox.allowlist_cap does not admit; not started: %w",
				agentID, d.Path, ErrSandboxPathRefused)
		}
		// Private: only this agent may reach a path it declared (the Windows
		// AppContainer scopes the grant to the agent's own capability).
		out = append(out, sandbox.Bind{Path: p, Write: d.Write, Private: true})
	}
	return out, nil
}

// runtimePrefix is the directory an interpreter needs read: a virtualenv's root
// for <venv>/bin/python or <venv>\Scripts\python.exe, else the executable's
// own directory. A "venv root" that would cover the sandbox's private /tmp
// (/tmp/bin/python -> /tmp, /bin/python -> /) is never bound: the executable's
// own directory is, so the agent still starts (ENG-1969).
func runtimePrefix(exe string) string {
	if exe == "" {
		return ""
	}
	if !filepath.IsAbs(exe) {
		if lp, err := exec.LookPath(exe); err == nil {
			exe = lp
		}
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(abs)
	switch strings.ToLower(filepath.Base(dir)) {
	case "bin", "scripts":
		if root := filepath.Dir(dir); !sandbox.CoversPrivateTmp(root) {
			return root
		}
	}
	return dir
}

// runtimePaths are the directories an interpreter needs read: its prefix
// (runtimePrefix) and, for a virtualenv, the base installation its pyvenv.cfg
// names ("home = <dir>"). Under bubblewrap the base is visible through the
// read-only root anyway; the Windows AppContainer reads only what is granted,
// and a venv's python.exe loads python3XX.dll from the base (ADR-0157 S5).
func runtimePaths(exe string) []string {
	prefix := runtimePrefix(exe)
	if prefix == "" {
		return nil
	}
	out := []string{prefix}
	raw, err := os.ReadFile(filepath.Join(prefix, "pyvenv.cfg"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "home" {
			continue
		}
		home := strings.TrimSpace(v)
		if !filepath.IsAbs(home) {
			break
		}
		// home is the directory holding the base interpreter: its bin/ or
		// Scripts\ parent on those layouts, the installation root otherwise.
		if b := strings.ToLower(filepath.Base(home)); b == "bin" || b == "scripts" {
			home = filepath.Dir(home)
		}
		if !sandbox.CoversPrivateTmp(home) {
			out = append(out, home)
		}
		break
	}
	return out
}

// agentBinds is everything an instance is shown at its own path: kernel-known
// read-only paths first, then the declared ones.
func (im *InstanceManager) agentBinds(ctx context.Context, def *domain.AgentDefinition, interp string) ([]sandbox.Bind, error) {
	sb := im.containment.Sandbox
	var declared []domain.SandboxPath
	if im.manifestFor != nil {
		if m := im.manifestFor(ctx, def.ID); m != nil {
			declared = m.SandboxPaths
		}
	}
	decl, err := sb.admitDeclared(def.ID, declared)
	if err != nil {
		return nil, err
	}
	var ro []string
	if def.Dir != "" {
		if abs, aerr := filepath.Abs(def.Dir); aerr == nil {
			ro = append(ro, abs)
		}
	}
	// A Python agent's runtime is exactly the managed env's current generation
	// (ADR-0157 D7.1.3), pre-granted when the build set the AppContainer ACE
	// (D7.7.5); any other interpreter's prefix (and venv base) as before.
	grantedHome, managed := "", false
	if def.Runtime == domain.RuntimePython {
		var home string
		var granted bool
		if home, granted, managed = im.pythonRuntimeBind(interp); managed {
			ro = append(ro, home)
			if granted {
				grantedHome = home
			}
			// ADR-0157 D7.1.3: the agent's own package layer, read-only and
			// private to it (the AppContainer grants it to this agent's
			// capability only; the other runners bind it into this sandbox only).
			lb, err := im.pythonLayerBind(def, home)
			if err != nil {
				return nil, err
			}
			if lb.Path != "" {
				decl = append(decl, lb)
			}
		}
	}
	if !managed {
		ro = append(ro, runtimePaths(interp)...)
	}
	ro = append(ro, sb.runtimeRead...)
	if im.agentTLSCA != "" {
		ro = append(ro, im.agentTLSCA)
	}
	if mc, ok := sb.modelCacheBind(def); ok {
		decl = append(decl, mc)
	}
	var binds []sandbox.Bind
	for _, p := range dedupePaths(ro) {
		binds = append(binds, sandbox.Bind{Path: p, Granted: grantedHome != "" && filepath.Clean(p) == grantedHome})
	}
	return append(binds, decl...), nil
}

// forwardNames are the variables the docker runner forwards into the
// container: every allowlisted variable except the host's OS essentials, which
// mean nothing inside a Linux image.
func forwardNames(env []string) []string {
	host := map[string]bool{}
	for _, n := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR",
		"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
		"HOMEDRIVE", "HOMEPATH", "SHELL", "USER", "USERNAME", "LOGNAME", "COMPUTERNAME", "OS",
		"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"} {
		host[procenv.Key(n)] = true
	}
	var out []string
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && !host[procenv.Key(kv[:i])] {
			out = append(out, kv[:i])
		}
	}
	return out
}

// sandboxLaunch is the sandbox spec and command for one instance: the tier
// from the kernel's view of the agent, the binds (code, runtime, kernel-known
// read paths, admitted manifest paths), the per-instance socket directory, and
// buildAgentCmd's argv and env unchanged.
//
// kernelSock reports whether the instance reaches the kernel through its
// kernel socket (kernelSocketTransport); the command then already dials it.
func (im *InstanceManager) sandboxLaunch(ctx context.Context, def *domain.AgentDefinition, inst *domain.Instance,
	desc *exec.Cmd, memMB int) (spec sandbox.Spec, c sandbox.Command, kernelSock bool, err error) {
	sb := im.containment.Sandbox
	binds, err := im.agentBinds(ctx, def, desc.Path)
	if err != nil {
		return sandbox.Spec{}, sandbox.Command{}, false, err
	}
	dir := desc.Dir
	if dir != "" {
		if abs, aerr := filepath.Abs(dir); aerr == nil {
			dir = abs
		}
	}
	spec = sandbox.Spec{
		ServerID:   def.ID + "-" + shortInstance(inst.ID),
		Tier:       sandboxTier(def),
		TierSource: "agent:" + agentTrustTier(def),
		Server:     sandbox.ServerSpec{MemoryMB: memMB, Image: sb.dockerImage},
		Policy:     sb.policy,
		Agent: &sandbox.AgentSpec{SocketDir: sb.socketDir(inst.ID), Binds: binds, DockerUser: sb.dockerUser,
			Ephemeral: true, Identity: def.ID},
	}
	c = sandbox.Command{Path: desc.Path, Args: desc.Args[1:], Env: desc.Env, Dir: dir, Forward: forwardNames(desc.Env)}
	if kernelSock = sb.kernelSocketTransport(ctx, spec, c); kernelSock {
		c = im.kernelOnlyCommand(c, sb.kernelSocketPath(inst.ID))
	}
	return spec, c, kernelSock, nil
}

// startSandboxed launches the command buildAgentCmd described through
// sandbox.StartCmd. It returns the started command (the caller Waits on it),
// its stdout/stderr pipes, and the cleanup to run once it has exited.
func (im *InstanceManager) startSandboxed(ctx context.Context, def *domain.AgentDefinition, inst *domain.Instance,
	desc *exec.Cmd, memMB int) (*exec.Cmd, io.ReadCloser, io.ReadCloser, func(), error) {
	spec, c, useKernelSock, err := im.sandboxLaunch(ctx, def, inst, desc, memMB)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sockDir := spec.Agent.SocketDir
	// ENG-2088: claim the instance's scratch and socket directory BEFORE they
	// exist, so a second kernel's boot sweep of a shared work root never takes
	// them; the claim goes once they are removed (removeDirs).
	owner := im.containment.Sandbox.workRootOwner()
	scratch := sandbox.WorkdirFor(spec.Policy, spec)
	if err := owner.claim(scratch, sockDir); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("agent %s: work root: %w", def.ID, err)
	}
	removeSockDir := func() {
		_ = os.RemoveAll(sockDir)
		owner.release(sockDir)
	}
	if err := mkdirPrivate(spec.Policy.WorkRoot, sockDir); err != nil {
		owner.release(scratch, sockDir)
		return nil, nil, nil, nil, fmt.Errorf("agent %s: socket directory: %w", def.ID, err)
	}
	// ADR-0157 S2: a kernel-only instance has no network; the kernel serves its
	// agent plane on a unix socket in the instance's socket directory, which the
	// sandbox already shows at its own path (bwrap bind, docker -v, Seatbelt
	// allow, the AppContainer's grant on the directory — S5).
	var kernelSock io.Closer
	if sb := im.containment.Sandbox; useKernelSock {
		if im.kernelSocket == nil {
			removeSockDir()
			owner.release(scratch)
			return nil, nil, nil, nil, fmt.Errorf("agent %s (%s) reaches the kernel only through its unix socket (a kernel-only network, ADR-0157 D3, or an AppContainer without loopback, S5) but the kernel has no agent socket to give it; not started",
				def.ID, agentTrustTier(def))
		}
		if kernelSock, err = im.kernelSocket(sb.kernelSocketPath(inst.ID)); err != nil {
			removeSockDir()
			owner.release(scratch)
			return nil, nil, nil, nil, fmt.Errorf("agent %s: kernel socket: %w", def.ID, err)
		}
	}
	closeKernelSock := func() {
		if kernelSock != nil {
			_ = kernelSock.Close()
		}
	}
	var stdout, stderr io.ReadCloser
	l, rep, err := sandbox.StartCmd(ctx, spec, c, func(cmd *exec.Cmd) error {
		var perr error
		if stdout, perr = cmd.StdoutPipe(); perr != nil {
			return fmt.Errorf("stdout pipe: %w", perr)
		}
		if stderr, perr = cmd.StderrPipe(); perr != nil {
			return fmt.Errorf("stderr pipe: %w", perr)
		}
		return nil
	})
	im.reportContainment(ctx, def, rep, err)
	if err != nil {
		closeKernelSock()
		removeSockDir()
		// StartCmd removes an ephemeral scratch it made when the start fails;
		// a scratch it could not remove is swept at the next boot.
		owner.release(scratch)
		var refused *sandbox.RefusedError
		if errors.As(err, &refused) {
			// A launch uid 65534 could not boot is refused on a degrade tier
			// too (ENG-2026); name on_unavailable only when it is the reason.
			why := ""
			if spec.Policy != nil && spec.Policy.Tiers[spec.Tier].OnUnavailable == sandbox.OnUnavailableRefuse {
				why = fmt.Sprintf(" (execution.agents.sandbox.%s.on_unavailable is refuse)", tierConfigKey(def))
			}
			return nil, nil, nil, nil, fmt.Errorf("agent %s (%s) not started: %w%s",
				def.ID, agentTrustTier(def), err, why)
		}
		return nil, nil, nil, nil, fmt.Errorf("agent %s: sandboxed start: %w", def.ID, err)
	}
	// Teardown paths (detachLocked: EvictInstance, ReleaseInstance, EvictAgent,
	// KillAllAgents) only KILL the tree, after releasing the manager lock; the
	// process's one Wait then runs cleanup, which collects out/ and releases the
	// sandbox (scratch removal) — ADR-0157 D5.1: nothing in the scratch survives
	// the instance except what the kernel took.
	im.mu.Lock()
	im.capCleanups[inst.ID] = func() { _ = l.Kill() }
	im.mu.Unlock()
	collectCtx := context.WithoutCancel(ctx) // outlives the boot request (AGENTS.md §2.7)
	agentID, instanceID := def.ID, inst.ID
	cleanup := func() {
		_ = l.Kill() // normally already exited; the collection must see no writer
		// ENG-2112: a limit the sandbox killed the instance over (scratch
		// quota, process count, memory) is named here and handed to the exit
		// observers (the daemon crash watcher's restart / quarantine).
		if reason := l.KillReason(); reason != "" {
			slog.WarnContext(collectCtx, "ADR-0157: agent instance killed by its sandbox limit",
				"agent", agentID, "instance", instanceID, "tier", agentTrustTier(def), "reason", reason)
			im.noteLimitKill(instanceID, reason)
		}
		// The docker runner's agent uid is not the kernel's (ENG-2031): its
		// files come back to the kernel before out/ is read. A failure keeps
		// the scratch on disk and says so; out/ is collected as far as it can.
		if err := l.Reclaim(collectCtx); err != nil {
			slog.ErrorContext(collectCtx, "ADR-0157: agent scratch not handed back to the kernel; out/ collection may be incomplete and the scratch is kept",
				"agent", agentID, "instance", instanceID, "dir", l.Workdir(), "err", err)
		}
		im.collectOutputs(collectCtx, agentID, instanceID, l.Workdir())
		l.Release()
		// A scratch Release kept (a failed docker reclaim) stays on disk
		// unclaimed: the next boot sweep removes it.
		owner.release(scratch)
		closeKernelSock()
		removeSockDir()
		im.mu.Lock()
		delete(im.capCleanups, inst.ID)
		im.mu.Unlock()
	}
	return l.Cmd, stdout, stderr, cleanup, nil
}

func tierConfigKey(def *domain.AgentDefinition) string {
	if agentTrustTier(def) == agentTierThirdParty {
		return "third_party"
	}
	return "first_party"
}

// reportContainment writes the per-agent enforcement report to the boot log
// (ADR-0157 D2.5): once per agent id at INFO when every wanted control is
// enforced, at WARN when it is degraded or refused; later spawns of the same
// agent log at DEBUG.
func (im *InstanceManager) reportContainment(ctx context.Context, def *domain.AgentDefinition, rep sandbox.Report, err error) {
	_, seen := im.reported.LoadOrStore(def.ID, true)
	attrs := []any{"agent_id", def.ID, "tier", agentTrustTier(def), "runner", rep.Runner,
		"enforced", controlNames(rep.Enforced), "missing", rep.Missing, "notes", rep.Notes}
	switch {
	case seen && err == nil:
		slog.DebugContext(ctx, "ADR-0157: agent containment", append(attrs, "summary", rep.Summary())...)
	case rep.Refused || err != nil:
		slog.WarnContext(ctx, "ADR-0157: agent containment REFUSED or failed", append(attrs, "summary", rep.Summary(), "err", err)...)
	case rep.Degraded():
		slog.WarnContext(ctx, "ADR-0157: agent started with DEGRADED containment", append(attrs, "summary", rep.Summary())...)
	default:
		slog.InfoContext(ctx, "ADR-0157: agent containment enforced", append(attrs, "summary", rep.Summary())...)
	}
}

func controlNames(cs []sandbox.Control) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// mkdirPrivate creates dir (0700) with every directory between root and it
// traverse-only (0711), so a dropped uid can reach its own socket directory
// without listing its neighbours'.
func mkdirPrivate(root, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o711); err != nil {
		return err
	}
	for d := filepath.Dir(dir); withinPath(d, root); d = filepath.Dir(d) {
		_ = os.Chmod(d, 0o711)
		if filepath.Clean(d) == filepath.Clean(root) {
			break
		}
	}
	return os.MkdirAll(dir, 0o700)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func expandAll(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.Clean(expandHome(p)))
		}
	}
	return out
}

// dedupePaths cleans, drops empty and repeated paths, keeping the first order.
func dedupePaths(ps []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		p = filepath.Clean(p)
		k := p
		if goruntime.GOOS == "windows" {
			k = strings.ToLower(k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// withinPath reports whether path is dir or lies under it.
func withinPath(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	p, d := filepath.Clean(path), filepath.Clean(dir)
	if goruntime.GOOS == "windows" {
		p, d = strings.ToLower(p), strings.ToLower(d)
	}
	if p == d {
		return true
	}
	if !strings.HasSuffix(d, string(filepath.Separator)) {
		d += string(filepath.Separator)
	}
	return strings.HasPrefix(p, d)
}
