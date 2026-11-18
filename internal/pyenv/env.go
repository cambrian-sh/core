package pyenv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
)

// FixCommand is the CLI that builds the env by hand (D7.11.2: the exact form is
// decided in P1).
const FixCommand = "cambrian-orchestrator python-env build"

// ErrBuilding is returned by Ready while a background build runs.
var ErrBuilding = errors.New("sandbox Python env building")

// Env is the kernel's view of the managed environment at run time: the boot
// check, the build_on_boot background build, and the readiness gate the agent
// launch path consults before spawning a Python agent (D7.11.2). It owns at
// most one background goroutine (Start/Stop, AGENTS.md §3.3).
type Env struct {
	b           *Builder
	buildOnBoot bool

	mu       sync.Mutex
	status   Status
	building bool
	step     string
	buildErr error

	cancel context.CancelFunc
	done   chan struct{}
}

// NewEnv wraps a builder. buildOnBoot is execution.agents.python.build_on_boot.
func NewEnv(b *Builder, buildOnBoot bool) *Env {
	return &Env{b: b, buildOnBoot: buildOnBoot, status: Status{State: StateMissing, Reason: "not checked yet"}}
}

// Root is the env root.
func (e *Env) Root() string { return e.b.Root }

// Builder is the env's builder: the per-agent package layers (P3) resolve and
// install with the same pins, sources and root.
func (e *Env) Builder() *Builder { return e.b }

// Start runs the boot check synchronously and, when the env is missing, broken
// or stale and build_on_boot is on, starts one background build. The kernel
// itself never waits for it: only Python agent spawns do (Ready).
func (e *Env) Start(ctx context.Context) {
	st, err := e.b.Check(ctx)
	if err != nil {
		st = Status{State: StateBroken, Reason: err.Error()}
	}
	e.mu.Lock()
	e.status = st
	e.mu.Unlock()
	switch {
	case st.State == StateHealthy:
		slog.Info("ADR-0157 D7: sandbox Python env healthy", "generation", st.Generation, "interpreter", st.Interpreter, "packages", st.Packages)
		return
	case !e.buildOnBoot && st.Usable():
		slog.Warn("ADR-0157 D7: sandbox Python env is from an older release and build_on_boot is off; Python agents keep running on it",
			"reason", st.Reason, "fix", FixCommand)
		return
	case !e.buildOnBoot:
		slog.Error("ADR-0157 D7: sandbox Python env unusable and execution.agents.python.build_on_boot is false; Python agents will be refused",
			"state", st.State, "reason", st.Reason, "fix", e.fix())
		return
	}
	slog.Warn("ADR-0157 D7: sandbox Python env needs a build; building in the background (Python agents wait for it)",
		"state", st.State, "reason", st.Reason, "root", e.b.Root)
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.mu.Lock()
	e.building, e.step, e.cancel, e.done = true, "starting", cancel, make(chan struct{})
	done := e.done
	e.mu.Unlock()
	go func() {
		defer close(done)
		e.runBuild(bctx)
	}()
}

func (e *Env) runBuild(ctx context.Context) {
	b := *e.b
	b.Progress = func(s string) {
		e.mu.Lock()
		e.step = s
		e.mu.Unlock()
		slog.Info("ADR-0157 D7: sandbox Python env build", "step", s)
	}
	res, err := b.Build(ctx)
	var st Status
	if err == nil {
		st, err = b.Check(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.building = false
	if err != nil {
		e.buildErr = err
		slog.Error("ADR-0157 D7: sandbox Python env build failed; Python agents refused until it is fixed",
			"err", err, "fix", e.fix())
		return
	}
	e.buildErr = nil
	e.status = st
	slog.Info("ADR-0157 D7: sandbox Python env ready", "generation", st.Generation, "interpreter", res.Interpreter,
		"packages", res.Packages, "reused", res.Reused)
}

// Stop cancels a running background build and waits for it.
func (e *Env) Stop() {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Wait blocks until a running background build finishes (it does not cancel
// it).
func (e *Env) Wait() {
	e.mu.Lock()
	done := e.done
	e.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Status is the last check or build result.
func (e *Env) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Ready is the launch gate: nil when Python agents may start, else the reason
// with the fix named. Never a fallback to the host's Python (D7.11.2).
func (e *Env) Ready() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readyLocked()
}

// Interpreter is the current base generation's interpreter for the next agent
// spawn (ADR-0157 D7 P2), or Ready's error when Python agents may not start.
// It is read on every spawn, so a background build that switches generations
// is picked up by the next instance while running ones keep theirs (D7.10.1).
func (e *Env) Interpreter() (Interpreter, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.readyLocked(); err != nil {
		return Interpreter{}, err
	}
	return Interpreter{Path: e.status.Interpreter, Home: e.status.Home, Granted: e.status.Granted}, nil
}

func (e *Env) readyLocked() error {
	switch {
	case e.status.Usable():
		return nil
	case e.building:
		return fmt.Errorf("%w (step: %s); Python agents start when it is ready", ErrBuilding, e.step)
	case e.buildErr != nil:
		return fmt.Errorf("sandbox Python env build failed: %v; fix: %s", e.buildErr, e.fix())
	}
	return fmt.Errorf("sandbox Python env %s (%s); fix: %s", e.status.State, e.status.Reason, e.fix())
}

func (e *Env) fix() string {
	return fmt.Sprintf("run `%s` (or re-run setup); without network place the offline bundle's archives in %s and set execution.agents.python.wheelhouse (and offline: true), or set execution.agents.python.python_mirror_url / index_url to an internal mirror",
		FixCommand, filepath.Join(e.b.Root, dirDownloads))
}
