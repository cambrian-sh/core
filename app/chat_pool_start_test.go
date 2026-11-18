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
}

func (p *fakeChatPool) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
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
}

func newDelayedPythonEnv(pi pyenv.Interpreter) *delayedPythonEnv {
	return &delayedPythonEnv{pi: pi, readyC: make(chan struct{})}
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

func (e *delayedPythonEnv) finish() {
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
	e.once.Do(func() { close(e.readyC) })
}

func (e *delayedPythonEnv) fail(err error) {
	e.mu.Lock()
	e.failed = err
	e.mu.Unlock()
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

func buildingErr() error {
	return fmt.Errorf("agentpool: spawn worker 1/2 for \"chat_agent\": SpawnDaemon: boot failed for \"chat_agent\": agent \"chat_agent\" not started: %w (step: verifying installer archive)", pyenv.ErrBuilding)
}

// ENG-2105: a start refused because the env is building waits for the env's
// readiness signal and starts the pool once — without a restart — and the lane's
// readiness names the env's step until then.
func TestChatPoolStarter_StartsWhenEnvBecomesReady(t *testing.T) {
	pool := &fakeChatPool{startErr: buildingErr()}
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	s := newChatPoolStarter(pool, env, nil)
	s.Start(context.Background())
	t.Cleanup(s.Stop)

	err := s.Ready()
	if err == nil || !strings.Contains(err.Error(), "waiting for the sandbox Python env") || !strings.Contains(err.Error(), "step: installing base packages") {
		t.Fatalf("readiness while the env builds: %v", err)
	}
	if n := pool.startCount(); n != 1 {
		t.Fatalf("starts before the env is ready: %d, want 1 (no polling)", n)
	}

	pool.set(nil)
	env.finish()
	waitChatUntil(t, "the pool to start after the env became ready", pool.Started)
	if err := s.Ready(); err != nil {
		t.Fatalf("readiness once the pool is up: %v", err)
	}
	if n := pool.startCount(); n != 2 {
		t.Fatalf("starts: %d, want exactly 2 (the refused one and the one on ready)", n)
	}
}

// A start that fails for any other reason is not retried and is reported as it is,
// with the boot layout problem attached.
func TestChatPoolStarter_OtherFailureIsReportedNotRetried(t *testing.T) {
	pool := &fakeChatPool{startErr: errors.New("boot failed: ModuleNotFoundError")}
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	s := newChatPoolStarter(pool, env, errors.New("prompt library not found"))
	s.Start(context.Background())
	t.Cleanup(s.Stop)
	env.finish()
	time.Sleep(50 * time.Millisecond)
	if n := pool.startCount(); n != 1 {
		t.Fatalf("a non-env failure was retried: %d starts", n)
	}
	err := s.Ready()
	if err == nil || !strings.Contains(err.Error(), "failed to start") || !strings.Contains(err.Error(), "ModuleNotFoundError") ||
		!strings.Contains(err.Error(), "prompt library not found") {
		t.Fatalf("readiness after a failed start: %v", err)
	}
}

// The env's build fails while the lane waits: the lane stays down, its reason
// becomes the build failure, and Stop ends the wait without a start.
func TestChatPoolStarter_BuildFailureWhileWaiting(t *testing.T) {
	pool := &fakeChatPool{startErr: buildingErr()}
	env := newDelayedPythonEnv(pyenv.Interpreter{Path: "python"})
	s := newChatPoolStarter(pool, env, nil)
	s.Start(context.Background())
	env.fail(errors.New("hash mismatch"))
	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "build failed: hash mismatch") {
		t.Fatalf("readiness after the build failed: %v", err)
	}
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

// A healthy env at boot: the pool starts at once and nothing waits.
func TestChatPoolStarter_ReadyEnvStartsAtOnce(t *testing.T) {
	pool := &fakeChatPool{}
	s := newChatPoolStarter(pool, newDelayedPythonEnv(pyenv.Interpreter{}), nil)
	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "not started yet") {
		t.Fatalf("before Start: %v", err)
	}
	s.Start(context.Background())
	if !pool.Started() || s.Ready() != nil {
		t.Fatalf("started=%v ready=%v", pool.Started(), s.Ready())
	}
	s.Stop()
	if pool.Started() {
		t.Fatal("Stop left the pool started")
	}
}

// With no managed env (nil gate) an ErrBuilding refusal cannot be waited on and
// is reported as a failure, never hangs.
func TestChatPoolStarter_NoEnvDoesNotWait(t *testing.T) {
	pool := &fakeChatPool{startErr: buildingErr()}
	s := newChatPoolStarter(pool, pythonReadinessOf(nil), nil)
	s.Start(context.Background())
	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("readiness: %v", err)
	}
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
