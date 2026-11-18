package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/internal/pyenv"
)

// chatLaneComponent is the chat lane's name in the health checker: the gRPC
// service "cambrian.lane.chat" and /healthz?component=chat (ENG-2105).
const chatLaneComponent = "chat"

// chatPoolRunner is the slice of agentpool.Pool the chat lane's start needs.
type chatPoolRunner interface {
	Start(context.Context) error
	Stop()
	Started() bool
}

// pythonReadiness is the slice of the managed Python env the chat lane's start
// needs: the explicit readiness signal and the gate's current refusal (its
// reason names the build step or the fix).
type pythonReadiness interface {
	Interpreter() (pyenv.Interpreter, error)
	ReadyC() <-chan struct{}
}

// chatPoolStarter starts the chat pool at boot and, when the start is refused
// because the sandbox Python env is still building, starts it once the env
// signals ready — no restart, no sleep loop (ENG-2105).
//
// Before, the chat-pool lifecycle called pool.Start once and only logged the
// error: on a fresh install the env build takes ~2 min, the pool started 6 s
// into it, and every turn answered "agent pool not started" until a restart.
// Rejected: retrying Start with a backoff — it polls a state the env already
// announces, and keeps spawning (and failing) Python processes while it waits.
//
// It owns at most one goroutine (Start/Stop, AGENTS.md §3.3): the wait for the
// env, which Stop cancels and joins before stopping the pool.
type chatPoolStarter struct {
	pool chatPoolRunner
	env  pythonReadiness // nil: no managed env to wait for
	// layout is the boot layout problem the chat agent will hit (a missing
	// prompt library), or nil; a failed start's reason carries it.
	layout error

	mu      sync.Mutex
	err     error // the last start failure; nil when started or not tried yet
	waiting bool  // a start was refused with ErrBuilding and waits for the env
	stop    chan struct{}
	done    chan struct{}
}

func newChatPoolStarter(pool chatPoolRunner, env pythonReadiness, layout error) *chatPoolStarter {
	return &chatPoolStarter{pool: pool, env: env, layout: layout}
}

// Start is the chat-pool Lifecycle.Start.
func (s *chatPoolStarter) Start(ctx context.Context) {
	err := s.pool.Start(ctx)
	if err == nil {
		return
	}
	if s.env == nil || !errors.Is(err, pyenv.ErrBuilding) {
		s.fail(err)
		return
	}
	slog.Warn("ADR-0084: chat pool waits for the sandbox Python env; the chat lane starts when the env is ready", "reason", err)
	stop, done := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.err, s.waiting, s.stop, s.done = err, true, stop, done
	s.mu.Unlock()
	ready := s.env.ReadyC()
	go func() {
		defer close(done)
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ready:
		}
		err := s.pool.Start(ctx)
		s.mu.Lock()
		s.waiting = false
		s.mu.Unlock()
		if err != nil {
			s.fail(err)
			return
		}
		s.mu.Lock()
		s.err = nil
		s.mu.Unlock()
		slog.Info("ADR-0084: chat pool started once the sandbox Python env was ready; chat lane available")
	}()
}

func (s *chatPoolStarter) fail(err error) {
	if s.layout != nil {
		err = fmt.Errorf("%w (%v)", err, s.layout)
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	slog.Error("ADR-0084: chat pool failed to start; chat lane unavailable", "err", err)
}

// Stop is the chat-pool Lifecycle.Stop: it ends a pending wait for the env
// (joining a start already under way), then stops the pool.
func (s *chatPoolStarter) Stop() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	s.pool.Stop()
}

// Ready is the chat lane's health component: nil once the pool is up, else
// why not — the env's build step while it waits, the build failure, or the
// start error. It never blocks (Pool.Started does not wait on a Start).
func (s *chatPoolStarter) Ready() error {
	if s.pool.Started() {
		return nil
	}
	s.mu.Lock()
	waiting, err := s.waiting, s.err
	s.mu.Unlock()
	if waiting {
		if _, envErr := s.env.Interpreter(); envErr != nil {
			return fmt.Errorf("chat pool waiting for the sandbox Python env: %w", envErr)
		}
		return errors.New("chat pool starting (the sandbox Python env is ready)")
	}
	if err != nil {
		return fmt.Errorf("chat pool failed to start: %w", err)
	}
	return errors.New("chat pool not started yet")
}

// promptLibraryMarker is the file the chat agent looks for to find the kernel
// prompt library (agents/chat_agent.py _prompts_root).
var promptLibraryMarker = filepath.Join("chat_agent", "v1.md")

// checkAgentLayout reports the directories the shipped Python agents need and
// do not find, each named with its fix (ENG-2105): the agents directory, and
// the prompt library the chat agent reads from disk — $CAMBRIAN_PROMPTS_DIR,
// else the nearest ancestor prompts/ of the agents directory (the layout
// `setup` unpacks: <prefix>/agents beside <prefix>/prompts). Without it the
// chat agent fails at import and the chat lane never comes up, which used to
// surface only as a generic boot failure. nil when the layout is complete.
func checkAgentLayout(agentsDir string) error {
	if strings.TrimSpace(agentsDir) == "" {
		return nil
	}
	abs := absOr(agentsDir)
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("agents directory %s not found: no shipped agent can start (set metabolism.agents_dir or re-run setup)", abs)
	}
	if env := strings.TrimSpace(os.Getenv("CAMBRIAN_PROMPTS_DIR")); env != "" {
		if _, err := os.Stat(filepath.Join(env, promptLibraryMarker)); err != nil {
			return fmt.Errorf("prompt library not found: CAMBRIAN_PROMPTS_DIR=%s has no %s", env, promptLibraryMarker)
		}
		return nil
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, "prompts", promptLibraryMarker)); err == nil {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return fmt.Errorf("prompt library not found: no prompts/%s in %s or any parent directory; the chat agent fails at import (put cambrian-core's prompts/ at %s — `setup` unpacks it there — or set CAMBRIAN_PROMPTS_DIR)",
		filepath.ToSlash(promptLibraryMarker), abs, filepath.Join(filepath.Dir(abs), "prompts"))
}

// pythonReadinessOf narrows the kernel's env gate; nil when there is none, so
// the starter does not wait on an env that was never installed.
func pythonReadinessOf(env pythonEnvGate) pythonReadiness {
	if env == nil {
		return nil
	}
	return env
}
