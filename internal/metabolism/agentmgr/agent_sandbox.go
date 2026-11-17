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
	"strings"

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
//   - read-only: its code directory, the configured interpreters' prefixes, the
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
}

// NewAgentSandbox builds the agent tiers' sandbox policy.
//
// Tiers (D1, D3): first-party (system agents run at it too) wants the env
// allowlist, a workdir, a killable tree, the memory cap, write confinement and
// hidden kernel state — and, when the kernel runs as root on Unix, a separate
// uid (D2.2). Third-party additionally always wants a low-privilege identity.
// Neither tier wants the network denied in S1: the agent must reach the kernel
// over its TCP plane until the kernel-only mode (S2) exists, so a third-party
// agent's missing network control is reported by admitLaunch instead.
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
				ConfineFS: true, HideState: true, LowPrivilege: rootUnix},
			sandbox.TierThirdParty: {OnUnavailable: onUnavail(o.ThirdPartyRefuse), Runner: runner,
				ConfineFS: true, HideState: true, LowPrivilege: true},
		},
		WorkRoot:     workRoot,
		WorkdirEnv:   map[string]string{"TEMP": "tmp", "TMP": "tmp", "TMPDIR": "tmp", "HOME": "home"},
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
	}
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
		out = append(out, sandbox.Bind{Path: p, Write: d.Write})
	}
	return out, nil
}

// runtimePrefix is the directory an interpreter needs read: a virtualenv's root
// for <venv>/bin/python or <venv>\Scripts\python.exe, else the executable's
// own directory.
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
		return filepath.Dir(dir)
	}
	return dir
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
	ro = append(ro, runtimePrefix(interp))
	ro = append(ro, sb.runtimeRead...)
	if im.agentTLSCA != "" {
		ro = append(ro, im.agentTLSCA)
	}
	var binds []sandbox.Bind
	for _, p := range dedupePaths(ro) {
		binds = append(binds, sandbox.Bind{Path: p})
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
func (im *InstanceManager) sandboxLaunch(ctx context.Context, def *domain.AgentDefinition, inst *domain.Instance,
	desc *exec.Cmd, memMB int) (sandbox.Spec, sandbox.Command, error) {
	sb := im.containment.Sandbox
	binds, err := im.agentBinds(ctx, def, desc.Path)
	if err != nil {
		return sandbox.Spec{}, sandbox.Command{}, err
	}
	dir := desc.Dir
	if dir != "" {
		if abs, aerr := filepath.Abs(dir); aerr == nil {
			dir = abs
		}
	}
	spec := sandbox.Spec{
		ServerID:   def.ID + "-" + shortInstance(inst.ID),
		Tier:       sandboxTier(def),
		TierSource: "agent:" + agentTrustTier(def),
		Server:     sandbox.ServerSpec{MemoryMB: memMB, Image: sb.dockerImage},
		Policy:     sb.policy,
		Agent: &sandbox.AgentSpec{SocketDir: sb.socketDir(inst.ID), Binds: binds, DockerUser: sb.dockerUser,
			Ephemeral: true},
	}
	c := sandbox.Command{Path: desc.Path, Args: desc.Args[1:], Env: desc.Env, Dir: dir, Forward: forwardNames(desc.Env)}
	return spec, c, nil
}

// startSandboxed launches the command buildAgentCmd described through
// sandbox.StartCmd. It returns the started command (the caller Waits on it),
// its stdout/stderr pipes, and the cleanup to run once it has exited.
func (im *InstanceManager) startSandboxed(ctx context.Context, def *domain.AgentDefinition, inst *domain.Instance,
	desc *exec.Cmd, memMB int) (*exec.Cmd, io.ReadCloser, io.ReadCloser, func(), error) {
	spec, c, err := im.sandboxLaunch(ctx, def, inst, desc, memMB)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sockDir := spec.Agent.SocketDir
	if err := mkdirPrivate(spec.Policy.WorkRoot, sockDir); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("agent %s: socket directory: %w", def.ID, err)
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
		_ = os.RemoveAll(sockDir)
		var refused *sandbox.RefusedError
		if errors.As(err, &refused) {
			return nil, nil, nil, nil, fmt.Errorf("agent %s (%s) not started: %w (execution.agents.sandbox.%s.on_unavailable is refuse)",
				def.ID, agentTrustTier(def), err, tierConfigKey(def))
		}
		return nil, nil, nil, nil, fmt.Errorf("agent %s: sandboxed start: %w", def.ID, err)
	}
	im.mu.Lock()
	im.capCleanups[inst.ID] = l.Release
	im.mu.Unlock()
	cleanup := func() {
		l.Release()
		_ = os.RemoveAll(sockDir)
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
