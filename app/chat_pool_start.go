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

// pythonReadiness is the slice of the managed Python env the chat lane's
// readiness reason needs: the gate's current refusal (its reason names the
// build step or the fix).
type pythonReadiness interface {
	Interpreter() (pyenv.Interpreter, error)
}

// chatPoolStarter starts the chat pool once a worker spawned now would be
// served on its first dial and could launch: after the kernel serves gRPC
// (ENG-2148) and the sandbox Python env is ready, or settled without becoming
// ready (ENG-2105, ENG-2137). It waits on the same gate the plugins' boot-time
// spawns wait on (spawnGate, KernelServices.AwaitSpawnReady), not a second one.
//
// Before ENG-2148 the chat-pool lifecycle called pool.Start straight away, and
// lifecycles start before serveGRPC reaches Serve: the workers dialled the
// early-bound listener and sat unserved in the OS backlog until Serve — the
// redial / CLOSE_WAIT window ENG-2135 closed for plugin daemons. Before
// ENG-2105 a start refused because the env was still building was only
// logged, and every turn answered "agent pool not started" until a restart.
// Rejected: moving the pool start after serveMarked in serveGRPC — the chat
// lane is a lifecycle like any other, and the gate already composes every
// precondition; retrying Start with a backoff — it polls states the kernel and
// the env already announce, and spawns (and fails) Python processes meanwhile.
//
// It owns at most one goroutine (Start/Stop, AGENTS.md §3.3): the wait for the
// gate and the start behind it, which Stop cancels and joins before stopping
// the pool.
type chatPoolStarter struct {
	pool chatPoolRunner
	gate spawnGate
	env  pythonReadiness // nil: no managed env to name in the readiness reason
	// layout is the boot layout problem the chat agent will hit (a missing
	// prompt library), or nil; a failed start's reason carries it.
	layout error

	mu      sync.Mutex
	err     error // the last start failure; nil when started or not tried yet
	waiting bool  // Start ran and the pool start waits on the gate
	cancel  context.CancelFunc
	done    chan struct{}
}

func newChatPoolStarter(pool chatPoolRunner, gate spawnGate, env pythonReadiness, layout error) *chatPoolStarter {
	return &chatPoolStarter{pool: pool, gate: gate, env: env, layout: layout}
}

// Start is the chat-pool Lifecycle.Start. It never blocks: the pool starts
// from the starter's goroutine once the gate opens.
func (s *chatPoolStarter) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.mu.Lock()
	s.waiting, s.cancel, s.done = true, cancel, done
	s.mu.Unlock()
	go func() {
		defer close(done)
		if err := s.gate.await(ctx); err != nil {
			return // shutdown before the gate opened: nothing was spawned
		}
		err := s.pool.Start(ctx)
		s.mu.Lock()
		s.waiting = false
		s.mu.Unlock()
		if err != nil {
			s.fail(err)
			return
		}
		slog.Info("ADR-0084: chat pool started once the kernel served and the sandbox Python env was ready; chat lane available")
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

// Stop is the chat-pool Lifecycle.Stop: it ends a pending wait on the gate
// (joining a start already under way), then stops the pool.
func (s *chatPoolStarter) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	s.pool.Stop()
}

// Ready is the chat lane's health component: nil once the pool is up, else
// why not — the kernel not serving yet, the env's build step while it waits,
// the build failure, or the start error. It never blocks (Pool.Started does
// not wait on a Start).
func (s *chatPoolStarter) Ready() error {
	if s.pool.Started() {
		return nil
	}
	s.mu.Lock()
	waiting, err := s.waiting, s.err
	s.mu.Unlock()
	if waiting {
		if !s.gate.serving() {
			return errors.New("chat pool waiting for the kernel to serve gRPC")
		}
		if s.env != nil {
			if _, envErr := s.env.Interpreter(); envErr != nil {
				return fmt.Errorf("chat pool waiting for the sandbox Python env: %w", envErr)
			}
		}
		return errors.New("chat pool starting (the kernel serves and the sandbox Python env is ready)")
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

// pythonReadinessOf narrows the kernel's env gate for the chat lane's readiness
// reason; nil when there is none, so the reason names no env never installed.
func pythonReadinessOf(env pythonEnvGate) pythonReadiness {
	if env == nil {
		return nil
	}
	return env
}
