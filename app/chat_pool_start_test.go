package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/pyenv"
)

// fakeChatPool refuses Start with startErr (read per call) and counts starts.
type fakeChatPool struct {
	mu       sync.Mutex
	startErr error
	starts   int
	started  bool
	stops    int
	onStart  func() // called at each Start, before the result
}

func (p *fakeChatPool) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	if p.onStart != nil {
		p.onStart()
	}
	if p.startErr != nil {
		return p.startErr
	}
	p.started = true
	return nil
}

func (p *fakeChatPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	p.started = false
}

func (p *fakeChatPool) Started() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

func (p *fakeChatPool) set(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startErr = err
}

func (p *fakeChatPool) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// delayedPythonEnv is a managed env whose build finishes when finish is called:
// the gate refuses with pyenv.ErrBuilding (naming its step) until then, and its
// readiness channel closes at that moment — the fake builder of ENG-2105.
type delayedPythonEnv struct {
	mu     sync.Mutex
	ready  bool
	failed error
	pi     pyenv.Interpreter
	readyC chan struct{}
	once   sync.Once
	// settledC closes when the build ends, healthy or failed (ENG-2137).
	settledC    chan struct{}
	settledOnce sync.Once
}

func newDelayedPythonEnv(pi pyenv.Interpreter) *delayedPythonEnv {
	return &delayedPythonEnv{pi: pi, readyC: make(chan struct{}), settledC: make(chan struct{})}
}

func (e *delayedPythonEnv) Interpreter() (pyenv.Interpreter, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.ready:
		return e.pi, nil
	case e.failed != nil:
		return pyenv.Interpreter{}, fmt.Errorf("sandbox Python env build failed: %v; fix: rebuild", e.failed)
	}
	return pyenv.Interpreter{}, fmt.Errorf("%w (step: installing base packages); Python agents start when it is ready", pyenv.ErrBuilding)
}

func (e *delayedPythonEnv) ReadyC() <-chan struct{} { return e.readyC }

// SettledC, Builder and Stop complete pythonEnvGate, so the fake also feeds the
// plugins' spawn gate through pythonSpawnReadinessOf (ENG-2137).
func (e *delayedPythonEnv) SettledC() <-chan struct{} { return e.settledC }
func (e *delayedPythonEnv) Builder() *pyenv.Builder   { return nil }
func (e *delayedPythonEnv) Stop()                     {}

func (e *delayedPythonEnv) finish() {
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
	e.once.Do(func() { close(e.readyC) })
	e.settledOnce.Do(func() { close(e.settledC) })
}

func (e *delayedPythonEnv) fail(err error) {
	e.mu.Lock()
	e.failed = err
	e.mu.Unlock()
	e.settledOnce.Do(func() { close(e.settledC) })
}

func waitChatUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openSignal is a serving signal already closed: the kernel serves.
func openSignal() *servingSignal {
	sig := newServingSignal()
	sig.markServing()
	return sig
}

// gateOf is the chat pool's gate as buildChatLane wires it.
func gateOf(sig *servingSignal, env pythonEnvGate) spawnGate {
	return spawnGate{Serving: sig.C(), PythonEnv: pythonSpawnReadinessOf(env)}
}

// ENG-2148 + ENG-2105: the pool starts behind the spawn gate — not while the
// kernel does not serve yet, not while the env builds — exactly once, with no
// refused attempt, and the lane's readiness names what it waits for.
func TestChatPoolStarter_WaitsForServingThenTheEnv(t *testing.T) {
	sig := newServingSignal()
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	pool := &fakeChatPool{}
	var servedAtStart, readyAtStart bool
	pool.onStart = func() {
		servedAtStart = isClosed(sig.C(), false)
		readyAtStart = isClosed(env.ReadyC(), false)
	}
	s := newChatPoolStarter(pool, gateOf(sig, env), env, nil)
	s.Start(context.Background())
	t.Cleanup(s.Stop)

	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "waiting for the kernel to serve") {
		t.Fatalf("readiness before the kernel serves: %v", err)
	}
	sig.markServing()
	waitChatUntil(t, "the readiness to name the env build", func() bool {
		err := s.Ready()
		return err != nil && strings.Contains(err.Error(), "waiting for the sandbox Python env") &&
			strings.Contains(err.Error(), "step: installing base packages")
	})
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 0 {
		t.Fatalf("starts while the env builds: %d, want 0 (no refused attempt)", n)
	}

	env.finish()
	waitChatUntil(t, "the pool to start once the gate opened", pool.Started)
	if err := s.Ready(); err != nil {
		t.Fatalf("readiness once the pool is up: %v", err)
	}
	if n := pool.startCount(); n != 1 {
		t.Fatalf("starts: %d, want exactly 1", n)
	}
	if !servedAtStart || !readyAtStart {
		t.Fatalf("pool started with serving=%v env ready=%v; want both", servedAtStart, readyAtStart)
	}
}

// The env is ready at boot but the kernel does not serve yet: nothing spawns
// until it does (ENG-2148: the workers' first dial must be served).
func TestChatPoolStarter_ReadyEnvWaitsForServing(t *testing.T) {
	sig := newServingSignal()
	env := newDelayedPythonEnv(pyenv.Interpreter{})
	env.finish()
	pool := &fakeChatPool{}
	s := newChatPoolStarter(pool, gateOf(sig, env), env, nil)
	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "not started yet") {
		t.Fatalf("before Start: %v", err)
	}
	s.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 0 {
		t.Fatalf("starts before the kernel serves: %d", n)
	}
	sig.markServing()
	waitChatUntil(t, "the pool to start once the kernel serves", pool.Started)
	s.Stop()
	if pool.Started() {
		t.Fatal("Stop left the pool started")
	}
}

// A start that fails for any other reason is not retried and is reported as it is,
// with the boot layout problem attached.
func TestChatPoolStarter_OtherFailureIsReportedNotRetried(t *testing.T) {
	pool := &fakeChatPool{startErr: errors.New("boot failed: ModuleNotFoundError")}
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	env.finish()
	s := newChatPoolStarter(pool, gateOf(openSignal(), env), env, errors.New("prompt library not found"))
	s.Start(context.Background())
	t.Cleanup(s.Stop)
	waitChatUntil(t, "the start to fail", func() bool {
		err := s.Ready()
		return err != nil && strings.Contains(err.Error(), "failed to start")
	})
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 1 {
		t.Fatalf("a failure was retried: %d starts", n)
	}
	err := s.Ready()
	if !strings.Contains(err.Error(), "ModuleNotFoundError") || !strings.Contains(err.Error(), "prompt library not found") {
		t.Fatalf("readiness after a failed start: %v", err)
	}
}

// The env's build fails while the lane waits: the gate opens (the env settled;
// waiting longer cannot help), the one start is refused with the build failure,
// and the lane reports it rather than waiting silently until shutdown.
func TestChatPoolStarter_BuildFailureWhileWaiting(t *testing.T) {
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	// What the launch gate answers a spawn once the build failed.
	pool := &fakeChatPool{startErr: errors.New("agentpool: spawn worker 1/2: sandbox Python env build failed: hash mismatch; fix: rebuild")}
	s := newChatPoolStarter(pool, gateOf(openSignal(), env), env, nil)
	s.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 0 {
		t.Fatalf("starts while the env builds: %d", n)
	}
	env.fail(errors.New("hash mismatch"))
	waitChatUntil(t, "the start to be refused with the build failure", func() bool {
		err := s.Ready()
		return err != nil && strings.Contains(err.Error(), "build failed: hash mismatch")
	})
	s.Stop()
	if n := pool.startCount(); n != 1 {
		t.Fatalf("starts: %d, want 1", n)
	}
	pool.mu.Lock()
	stops := pool.stops
	pool.mu.Unlock()
	if stops != 1 {
		t.Fatalf("pool stops: %d", stops)
	}
}

// Stop before the gate opens ends the wait: nothing is spawned, the goroutine
// is joined.
func TestChatPoolStarter_StopBeforeTheGateOpens(t *testing.T) {
	sig := newServingSignal()
	pool := &fakeChatPool{}
	s := newChatPoolStarter(pool, gateOf(sig, nil), nil, nil)
	s.Start(context.Background())
	s.Stop()
	sig.markServing()
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 0 {
		t.Fatalf("starts after Stop: %d", n)
	}
}

// With no managed env and no serving signal (a zero gate) the pool starts at
// once and a refusal is reported as a failure, never hangs.
func TestChatPoolStarter_NoGateDoesNotWait(t *testing.T) {
	pool := &fakeChatPool{startErr: errors.New("sandbox Python env unavailable: unpinned platform")}
	s := newChatPoolStarter(pool, spawnGate{}, pythonReadinessOf(nil), nil)
	s.Start(context.Background())
	waitChatUntil(t, "the start to fail", func() bool {
		err := s.Ready()
		return err != nil && strings.Contains(err.Error(), "failed to start")
	})
	s.Stop()
}

// ENG-2105: the boot layout check names a missing agents directory or prompt
// library with its fix, and accepts the layouts setup and the repo use.
func TestCheckAgentLayout(t *testing.T) {
	t.Setenv("CAMBRIAN_PROMPTS_DIR", "")
	root := t.TempDir()
	agents := filepath.Join(root, "agents")

	if err := checkAgentLayout(agents); err == nil || !strings.Contains(err.Error(), "agents directory") || !strings.Contains(err.Error(), agents) {
		t.Fatalf("missing agents dir: %v", err)
	}
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkAgentLayout(agents)
	if err == nil || !strings.Contains(err.Error(), "prompt library not found") || !strings.Contains(err.Error(), filepath.Join(root, "prompts")) ||
		!strings.Contains(err.Error(), "CAMBRIAN_PROMPTS_DIR") {
		t.Fatalf("missing prompts: %v", err)
	}
	// setup's layout: <prefix>/prompts beside <prefix>/agents.
	if err := os.MkdirAll(filepath.Join(root, "prompts", "chat_agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prompts", "chat_agent", "v1.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkAgentLayout(agents); err != nil {
		t.Fatalf("complete layout: %v", err)
	}
	// An explicit CAMBRIAN_PROMPTS_DIR wins, and is checked.
	t.Setenv("CAMBRIAN_PROMPTS_DIR", t.TempDir())
	if err := checkAgentLayout(agents); err == nil || !strings.Contains(err.Error(), "CAMBRIAN_PROMPTS_DIR=") {
		t.Fatalf("empty CAMBRIAN_PROMPTS_DIR: %v", err)
	}
	t.Setenv("CAMBRIAN_PROMPTS_DIR", filepath.Join(root, "prompts"))
	if err := checkAgentLayout(agents); err != nil {
		t.Fatalf("CAMBRIAN_PROMPTS_DIR set: %v", err)
	}
	// The shipped repo layout (cambrian-core/agents beside cambrian-core/prompts).
	t.Setenv("CAMBRIAN_PROMPTS_DIR", "")
	if err := checkAgentLayout(filepath.Join("..", "agents")); err != nil {
		t.Fatalf("repo layout: %v", err)
	}
}
