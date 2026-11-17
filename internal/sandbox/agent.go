package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The non-stdio launch (ADR-0157 D1/D2, stage S1): a kernel-spawned AGENT runs
// under the same Policy, runners and Report as an MCP server, but it does not
// speak over stdin/stdout. It serves a unix socket and its stdout/stderr are
// log lines, so the caller keeps exec.Cmd semantics: it attaches its own pipes
// before the start (prepare), owns the one Wait (crash detection), and calls
// Release once the process has exited.
//
// What only an agent has travels in Spec.Agent:
//   - a per-instance socket directory, shown at its own path so the kernel and
//     the agent name the same socket (D2.6: the --socket argv contract is
//     unchanged);
//   - path binds: the agent's code and runtime (read-only), and the paths its
//     manifest declared and the operator's allowlist_cap admitted (D1.2).
//
// The policy's HidePaths are the D1 hide-list; a tier that wants hide_state
// gets them unreadable where the runner can do that (bubblewrap, docker), and
// reported MISSING where it cannot (Windows low integrity confines writes only).

// AgentSpec is the agent half of a Spec.
type AgentSpec struct {
	// SocketDir is the instance's socket directory (created by the caller).
	SocketDir string
	// Binds are shown to the agent at their own path.
	Binds []Bind
	// DockerUser is the docker runner's --user ("" ⇒ 65534:65534): a separate
	// non-root identity, never the kernel's (D2.1).
	DockerUser string
	// Ephemeral removes the instance's workdir (its scratch) on Release (D1.2).
	Ephemeral bool
}

// Bind is one path an agent sees at its own path: read-only unless Write.
type Bind struct {
	Path  string
	Write bool
}

// Launched is one agent process started by StartCmd.
type Launched struct {
	// Cmd is the started command: the caller Waits on it (once) and reads the
	// pipes its prepare attached. Under bubblewrap or docker it is the wrapper
	// (bwrap / the docker CLI); Kill stops the whole contained tree.
	Cmd     *exec.Cmd
	h       osHandle
	report  Report
	workdir string
	remove  bool
	once    sync.Once
}

// Report is the launch's effective containment.
func (l *Launched) Report() Report { return l.report }

// Workdir is the instance's scratch directory on the host.
func (l *Launched) Workdir() string { return l.workdir }

// Kill stops the whole contained tree. Idempotent.
func (l *Launched) Kill() error { return l.h.kill() }

// KillReason is why OS-level containment killed the tree ("" if it did not).
func (l *Launched) KillReason() string { return l.h.limitReason() }

// Release kills what is left of the tree, frees OS resources (job object,
// cgroup, container) and removes an ephemeral scratch directory. Call it after
// the process has exited (or to end it). Idempotent.
func (l *Launched) Release() {
	l.once.Do(func() {
		if err := l.h.kill(); err != nil {
			slog.Debug("sandbox: kill on release", "agent", l.report.ServerID, "err", err)
		}
		l.h.release()
		if l.remove && l.workdir != "" {
			if err := os.RemoveAll(l.workdir); err != nil {
				slog.Warn("sandbox: agent scratch directory not removed", "agent", l.report.ServerID,
					"dir", l.workdir, "err", err)
			}
		}
	})
}

// ErrNotAgent is returned by StartCmd for a spec without Spec.Agent.
var ErrNotAgent = errors.New("sandbox: StartCmd needs an agent spec (Spec.Agent)")

// StartCmd launches c under spec's containment WITHOUT taking its stdio: the
// non-stdio Start variant for kernel-spawned agents (ADR-0157 D2). prepare runs
// on the built command before it starts (attach StdoutPipe/StderrPipe there);
// the caller then owns Cmd.Wait and must call Release after the exit. A spec
// whose tier refuses a missing control returns *RefusedError and starts nothing.
func StartCmd(ctx context.Context, spec Spec, c Command, prepare func(*exec.Cmd) error) (*Launched, Report, error) {
	if spec.Agent == nil {
		return nil, Report{}, ErrNotAgent
	}
	spec, p := spec.effective()
	if p.Disabled {
		return nil, Report{ServerID: spec.ServerID, Tier: spec.Tier, Disabled: true},
			errors.New("sandbox: StartCmd with a disabled policy (the caller launches uncontained itself)")
	}
	runner, rep, tp := choose(ctx, spec, p, c)
	if rep.Refused {
		return nil, rep, &RefusedError{Report: rep}
	}
	if kept, skipped := privateTmpSafe(spec.Agent.Binds); len(skipped) > 0 {
		a := *spec.Agent // the caller's spec is not changed
		a.Binds = kept
		spec.Agent = &a
		for _, s := range skipped {
			rep.Notes = append(rep.Notes, "bind "+s+" skipped: it would cover the agent's private /tmp")
		}
	}
	workdir, err := prepareWorkdir(p, spec)
	if err != nil {
		return nil, rep, fmt.Errorf("sandbox: workdir for %s: %w", spec.ServerID, err)
	}
	var l *Launched
	start := func(cmd *exec.Cmd, post func() (osHandle, error)) error {
		if prepare != nil {
			if err := prepare(cmd); err != nil {
				return err
			}
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		h, err := post()
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		l = &Launched{Cmd: cmd, h: h, report: rep, workdir: workdir, remove: spec.Agent.Ephemeral}
		return nil
	}
	if err := runnerStart(ctx, runner, p, spec, tp, c, workdir, start); err != nil {
		if spec.Agent.Ephemeral {
			_ = os.RemoveAll(workdir)
		}
		return nil, rep, err
	}
	return l, rep, nil
}

// CoversPrivateTmp reports whether a bind at path would cover the agent's
// private /tmp: "/", "/tmp" or "/tmp/work", in slash form (ENG-1969).
// Bubblewrap mounts a fresh tmpfs at /tmp and the instance's workdir at
// /tmp/work inside it; a read-only bind of /tmp (or of /) laid over that makes
// the workdir mount fail and the agent never starts, and would show the host's
// /tmp to the agent besides. Docker's --tmpfs /tmp clashes the same way. Such a
// bind is never made; a caller that needs something under it binds that
// subdirectory instead (agentmgr binds an interpreter's own directory).
func CoversPrivateTmp(path string) bool {
	if path == "" {
		return false
	}
	switch filepath.ToSlash(filepath.Clean(path)) {
	case "/", "/tmp", "/tmp/work":
		return true
	}
	return false
}

// privateTmpSafe splits binds into those an agent may get and the paths of
// those that would cover its private /tmp (CoversPrivateTmp).
func privateTmpSafe(binds []Bind) (kept []Bind, skipped []string) {
	for _, b := range binds {
		if CoversPrivateTmp(b.Path) {
			skipped = append(skipped, b.Path)
			continue
		}
		kept = append(kept, b)
	}
	return kept, skipped
}

// within reports whether path is dir or lies under it (both cleaned, absolute).
func within(path, dir string) bool {
	if dir == "" || path == "" {
		return false
	}
	p, d := filepath.Clean(path), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
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

// splitBinds orders an agent's binds around the hide-list: a bind outside every
// hidden path is mounted BEFORE the hides (so a hidden path inside it is still
// masked), a bind inside a hidden path AFTER them (it re-exposes that one
// subtree: the agent's code inside the kernel directory). Missing paths are
// dropped and named.
func splitBinds(binds []Bind, hide []string) (before, after []Bind, missing []string) {
	for _, b := range binds {
		if b.Path == "" {
			continue
		}
		if _, err := os.Stat(b.Path); err != nil {
			missing = append(missing, b.Path)
			continue
		}
		inside := false
		for _, h := range hide {
			if within(b.Path, h) {
				inside = true
				break
			}
		}
		if inside {
			after = append(after, b)
		} else {
			before = append(before, b)
		}
	}
	return before, after, missing
}
