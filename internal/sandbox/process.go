package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/internal/procenv"
)

// Command is what to launch. Env is the procenv-built environment (the D8
// allowlist plus this server's own settings and secrets); Start only ADDS the
// workdir variables to it, never anything from the kernel's environment.
type Command struct {
	Path string
	Args []string
	Env  []string
	// Forward names the server's OWN variables (passthrough + explicit), the
	// only ones a container runner forwards: the host's OS essentials (PATH,
	// SYSTEMROOT…) mean nothing inside a Linux image.
	Forward []string
	// Dir is the process's working directory as it sees it; "" ⇒ its workdir
	// (an MCP server). An agent runs in its own code directory (StartCmd).
	Dir string
}

// ErrOutputCap is returned by Read when one stdout message exceeds the cap.
var ErrOutputCap = errors.New("sandbox: output cap exceeded")

// osHandle is the runner-specific half of a launched process.
type osHandle interface {
	// kill stops the whole process tree. Idempotent.
	kill() error
	// release frees OS resources (job handle, cgroup) after the tree is gone.
	release()
	// limitReason is why the OS-level containment killed the tree ("" if it
	// did not): a Windows job memory or CPU-time limit.
	limitReason() string
}

// shutdownGrace is how long Close waits after closing stdin before killing
// the tree (the MCP stdio shutdown sequence: close input, wait, terminate).
const shutdownGrace = 2 * time.Second

// Process is one contained process speaking over stdin/stdout. It is an
// io.ReadWriteCloser: Read enforces the output cap and feeds the stall
// watchdog; Close runs the shutdown sequence and always kills the tree.
//
// Goroutines: one waiter (cmd.Wait) and one stall watchdog, both owned here,
// both drained by Close (goleak-tested).
type Process struct {
	cmd    *exec.Cmd
	h      osHandle
	stdin  *os.File
	stdout *os.File
	report Report

	maxLine int   // 0 ⇒ no cap
	lineLen int   // bytes since the last '\n'; Read is single-reader
	stall   int64 // ns; 0 ⇒ no stall kill
	// waitingSince is when a request was written with no output since (unix
	// ns); 0 when nothing is outstanding.
	waitingSince atomic.Int64

	done    chan struct{}
	waitErr error
	stop    chan struct{}
	wg      sync.WaitGroup

	killMu     sync.Mutex
	killReason string

	closeOnce sync.Once
	closeErr  error
}

// Report is the process's effective containment.
func (p *Process) Report() Report { return p.report }

// Pid is the launched process's id (the docker CLI's for the docker runner).
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Done is closed when the launched process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// KillReason is why the sandbox killed the process ("" if it did not).
func (p *Process) KillReason() string {
	p.killMu.Lock()
	r := p.killReason
	p.killMu.Unlock()
	if r == "" {
		r = p.h.limitReason()
	}
	return r
}

// Read implements io.Reader over the process's stdout.
func (p *Process) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	if n > 0 {
		p.waitingSince.Store(0)
		if p.maxLine > 0 {
			if i := bytes.LastIndexByte(b[:n], '\n'); i >= 0 {
				p.lineLen = n - i - 1
			} else {
				p.lineLen += n
			}
			if p.lineLen > p.maxLine {
				p.killWith(fmt.Sprintf("output_cap: one message exceeded %d KiB", p.maxLine/1024))
				return 0, ErrOutputCap
			}
		}
	}
	return n, err
}

// Write implements io.Writer over the process's stdin. A JSON-RPC request
// (it carries both "method" and "id") arms the stall watchdog; a
// notification or a response expects no answer and does not.
func (p *Process) Write(b []byte) (int, error) {
	if p.stall > 0 && p.waitingSince.Load() == 0 &&
		bytes.Contains(b, []byte(`"method"`)) && bytes.Contains(b, []byte(`"id"`)) {
		p.waitingSince.CompareAndSwap(0, time.Now().UnixNano())
	}
	return p.stdin.Write(b)
}

// Close closes stdin, gives the process shutdownGrace to exit, then kills the
// tree, drains the goroutines and releases OS resources. Idempotent.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(shutdownGrace):
		}
		// Always: a main process that exited may have left children running.
		if err := p.h.kill(); err != nil {
			slog.Debug("sandbox: kill on close", "server", p.report.ServerID, "err", err)
		}
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			p.closeErr = fmt.Errorf("sandbox: %s did not exit after kill", p.report.ServerID)
		}
		close(p.stop)
		p.wg.Wait()
		_ = p.stdout.Close()
		p.h.release()
	})
	return p.closeErr
}

func (p *Process) killWith(reason string) {
	p.killMu.Lock()
	first := p.killReason == ""
	if first {
		p.killReason = reason
	}
	p.killMu.Unlock()
	if first {
		slog.Warn("sandbox: killed a contained process", "server", p.report.ServerID,
			"tier", p.report.Tier, "reason", reason)
	}
	_ = p.h.kill()
}

func (p *Process) watchStall() {
	defer p.wg.Done()
	tick := time.Duration(p.stall) / 10
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-p.done:
			return
		case now := <-t.C:
			since := p.waitingSince.Load()
			if since != 0 && now.UnixNano()-since > p.stall {
				p.killWith(fmt.Sprintf("stall_kill: a request went unanswered for %s", time.Duration(p.stall)))
				return
			}
		}
	}
}

// Plan reports the containment a spec WOULD get on this platform without
// starting anything (status surfaces, tests). Docker availability is probed.
func Plan(ctx context.Context, spec Spec, c Command) Report {
	spec, p := spec.effective()
	if p.Disabled {
		return Report{ServerID: spec.ServerID, Tier: spec.Tier, TierSource: spec.TierSource, Disabled: true}
	}
	_, rep, _ := choose(ctx, spec, p, c)
	return rep
}

// choose picks the runner and plans the controls.
func choose(ctx context.Context, spec Spec, p *Policy, c Command) (string, Report, TierPolicy) {
	tp := spec.tierPolicy(p)
	runner := tp.Runner
	var capab capability
	switch runner {
	case RunnerDocker:
		capab = dockerCapability(ctx, p, spec, tp, c)
		if capab.unusable != "" {
			if tp.OnUnavailable != OnUnavailableDegrade {
				return runner, Report{ServerID: spec.ServerID, Tier: spec.Tier, TierSource: spec.TierSource,
					Runner: runner, Refused: true, Missing: []string{"runner: docker unavailable: " + capab.unusable}}, tp
			}
			reason := capab.unusable
			runner = RunnerNative
			capab = nativeCapability(p, spec, tp)
			capab.notes = append(capab.notes, "runner: docker unavailable ("+reason+"), fell back to native")
		}
	case RunnerNative:
		capab = nativeCapability(p, spec, tp)
	default:
		return runner, Report{ServerID: spec.ServerID, Tier: spec.Tier, TierSource: spec.TierSource,
			Runner: runner, Refused: true, Missing: []string{"runner: unknown runner " + runner}}, tp
	}
	return runner, plan(spec, tp, capab), tp
}

// Start launches c under spec's containment. The process is NOT bound to ctx
// (ctx only bounds the start-up probes): a server outlives the connect call
// that started it, and Close is its end.
func Start(ctx context.Context, spec Spec, c Command) (*Process, Report, error) {
	spec, p := spec.effective()
	if p.Disabled {
		rep := Report{ServerID: spec.ServerID, Tier: spec.Tier, TierSource: spec.TierSource, Disabled: true}
		slog.Warn("sandbox: containment disabled by config; launching with the env allowlist only",
			"server", spec.ServerID, "tier", spec.Tier)
		cmd := exec.Command(c.Path, c.Args...) //nolint:gosec // operator-configured command
		cmd.Env = c.Env
		proc, err := launch(cmd, plainHandle(cmd), rep, TierPolicy{})
		return proc, rep, err
	}
	runner, rep, tp := choose(ctx, spec, p, c)
	if rep.Refused {
		return nil, rep, &RefusedError{Report: rep}
	}
	workdir, err := prepareWorkdir(p, spec)
	if err != nil {
		return nil, rep, fmt.Errorf("sandbox: workdir for %s: %w", spec.ServerID, err)
	}
	var proc *Process
	start := func(cmd *exec.Cmd, post func() (osHandle, error)) error {
		var lerr error
		proc, lerr = launch(cmd, post, rep, tp)
		return lerr
	}
	if err := runnerStart(ctx, runner, p, spec, tp, c, workdir, start); err != nil {
		return nil, rep, err
	}
	if rep.Degraded() {
		slog.Warn("sandbox: started with degraded containment", "server", spec.ServerID,
			"tier", spec.Tier, "missing", strings.Join(rep.Missing, "; "))
	}
	return proc, rep, nil
}

// starter starts a command a runner built and runs the runner's post-start
// step (job assignment and resume on Windows, the cgroup fd on Linux). The
// stdio launch (launch, for MCP servers) and the detached launch (StartCmd,
// for agents) are the two starters; every runner builds its command once, for
// both (ADR-0157 D2: one sandbox, never a second one for agents).
type starter func(cmd *exec.Cmd, post func() (osHandle, error)) error

// runnerStart builds c under the chosen runner and hands it to start.
func runnerStart(ctx context.Context, runner string, p *Policy, spec Spec, tp TierPolicy, c Command, workdir string, start starter) error {
	if runner == RunnerDocker {
		return startDocker(ctx, p, spec, tp, c, workdir, start)
	}
	return startNative(p, spec, tp, c, workdir, start)
}

// launch starts cmd with pipe stdio and the supervising goroutines. post runs
// after Start (job assignment and resume on Windows, cgroup on Linux); a post
// failure kills what was started.
func launch(cmd *exec.Cmd, post func() (osHandle, error), rep Report, tp TierPolicy) (*Process, error) {
	// os.Pipe, not cmd.StdoutPipe: exec's Wait closes its own pipes the moment
	// the process exits, racing the reader for the final message. With our own
	// files the reader sees every byte, then EOF.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	// stderr: only a bounded tail is kept, for the exit diagnostic.
	tail := &tailBuffer{max: 4096}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	// The child holds its ends now.
	_ = inR.Close()
	_ = outW.Close()
	h, err := post()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = inW.Close()
		_ = outR.Close()
		return nil, err
	}
	proc := &Process{
		cmd: cmd, h: h, stdin: inW, stdout: outR, report: rep,
		maxLine: tp.MaxMessageKB * 1024,
		stall:   int64(time.Duration(tp.StallTimeoutSec) * time.Second),
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	proc.wg.Add(1)
	go func() {
		defer proc.wg.Done()
		proc.waitErr = cmd.Wait()
		close(proc.done)
		if proc.waitErr != nil {
			slog.Debug("sandbox: contained process exited", "server", rep.ServerID, "err", proc.waitErr,
				"stderr_tail", tail.String())
		}
	}()
	if proc.stall > 0 {
		proc.wg.Add(1)
		go proc.watchStall()
	}
	return proc, nil
}

// plainHandle kills only the direct child: the disabled-containment path.
func plainHandle(cmd *exec.Cmd) func() (osHandle, error) {
	return func() (osHandle, error) { return processHandle{cmd}, nil }
}

type processHandle struct{ cmd *exec.Cmd }

func (h processHandle) kill() error {
	if h.cmd.Process == nil {
		return nil
	}
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
func (processHandle) release()            {}
func (processHandle) limitReason() string { return "" }

// WorkdirFor is a server's working directory: <work_root>/<tier>/<id>.
func WorkdirFor(p *Policy, spec Spec) string {
	return filepath.Join(p.WorkRoot, string(spec.Tier), safeName(spec.ServerID))
}

func prepareWorkdir(p *Policy, spec Spec) (string, error) {
	if p.WorkRoot == "" {
		return "", errors.New("mcp.sandbox.work_root is empty")
	}
	// The root and tier levels are traverse-only for others (0711), so a
	// process dropped to another uid can reach its own 0700 workdir without
	// being able to list its neighbours.
	tierDir := filepath.Join(p.WorkRoot, string(spec.Tier))
	if err := os.MkdirAll(tierDir, 0o711); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone; restate it.
	_ = os.Chmod(p.WorkRoot, 0o711)
	_ = os.Chmod(tierDir, 0o711)
	dir := WorkdirFor(p, spec)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// safeName maps a server id onto one path segment / container-name fragment.
func safeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), ".")
	if s == "" {
		s = "server"
	}
	return s
}

// workdirEnv returns env with the workdir variables set (each to a created
// subdirectory of base). confined adds the ConfinedEnv set. dirBase is where
// the directories are created on the host; valueBase is the path the process
// sees (they differ inside a container).
func workdirEnv(p *Policy, env []string, confined bool, dirBase, valueBase string, sep string) ([]string, error) {
	set := map[string]string{}
	for k, v := range p.WorkdirEnv {
		set[k] = v
	}
	if confined {
		for k, v := range p.ConfinedEnv {
			set[k] = v
		}
	}
	if len(set) == 0 {
		return env, nil
	}
	for _, rel := range set {
		if err := os.MkdirAll(filepath.Join(dirBase, filepath.FromSlash(rel)), 0o700); err != nil {
			return nil, err
		}
	}
	return setEnv(env, set, valueBase, sep), nil
}

func setEnv(env []string, set map[string]string, valueBase, sep string) []string {
	keys := map[string]bool{}
	for k := range set {
		keys[procenv.Key(k)] = true
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && keys[procenv.Key(kv[:i])] {
			continue
		}
		out = append(out, kv)
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, k+"="+valueBase+sep+strings.ReplaceAll(set[k], "/", sep))
	}
	return out
}

// tailBuffer keeps the last max bytes written (a contained process's stderr,
// for diagnostics only: it may carry anything the server prints, so it is
// logged at debug level and never returned to a caller).
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
