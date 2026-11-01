package agentmgr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// R8 in the debugging playbook: an agent process that starts but never opens
// its UDS socket (T-20261031-documented-production-failure-modes-have-no), and
// its sibling — an agent process that EXITS before opening it
// (T-20261031-bootagent-waits-the-full-10-s-boottimeout-even). Guards
// process_exit.go awaitSocket:
//
//	case <-exit.done: return "… process exited before opening socket …"
//	if time.Now().After(deadline) { _ = cmd.Process.Kill(); return fmt.Errorf("… did not appear within %s") }
//	case <-ctx.Done(): _ = cmd.Process.Kill(); return ctx.Err()
//
// The child is this test binary launched as a RuntimeBinary agent, in one of two
// shapes:
//
//   - exiting: bootAgent hands it `--socket … --substrate-addr …`, which the Go
//     test flag parser rejects, so the child exits at once with status 2 — the
//     observable of an agent that dies in import (missing dependency, bad
//     entrypoint, unknown flag);
//   - lingering: with AGENTMGR_CHAOS_HELPER=sleep in its (allowlisted) env,
//     TestMain below sleeps BEFORE the flag parser runs, so the child stays
//     alive and never creates the socket — the observable of an agent that
//     hangs in import. It lives until bootAgent kills it.

// chaosSleepEnv is the InstanceManager extra that turns the child into the
// lingering shape. It travels through SetEnvExtras because agent env is
// allowlisted, never inherited (SEC-01).
const chaosSleepEnv = "AGENTMGR_CHAOS_HELPER=sleep"

func TestMain(m *testing.M) {
	if os.Getenv("AGENTMGR_CHAOS_HELPER") == "sleep" {
		// A child that never opens its socket. Sleeping here, before m.Run()
		// parses flags, is what keeps the `--socket` argument from ending the
		// process. Killed by the boot path; the sleep is only a backstop.
		time.Sleep(10 * time.Minute)
		return
	}
	os.Exit(m.Run())
}

func noSocketAgent() *domain.AgentDefinition {
	return &domain.AgentDefinition{
		ID:       "chaos-never-opens-socket",
		Runtime:  domain.RuntimeBinary,
		ExecPath: os.Args[0],
		Trait:    domain.TraitCognitive,
	}
}

// shortBootTimeout shortens agentBootTimeout for one test and restores it.
func shortBootTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := agentBootTimeout
	agentBootTimeout = d
	t.Cleanup(func() { agentBootTimeout = prev })
}

func assertNothingRegistered(t *testing.T, im *InstanceManager, def *domain.AgentDefinition) {
	t.Helper()
	im.mu.Lock()
	defer im.mu.Unlock()
	if n := len(im.instances); n != 0 {
		t.Fatalf("a failed boot must not register an instance, found %d", n)
	}
	if ids := im.agentIndex[def.ID]; len(ids) != 0 {
		t.Fatalf("a failed boot must not index the agent, found %v", ids)
	}
	if n := len(im.exits); n != 0 {
		t.Fatalf("a failed boot must not keep its exit watcher registered, found %d", n)
	}
}

// A child that exits before opening its socket fails the boot AT ONCE, naming
// the exit — not after the full timeout as "socket did not appear".
func TestChaos_AgentBoot_ChildExitFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches a real subprocess; skipped in -short")
	}
	im := NewInstanceManager("", "127.0.0.1:1")
	def := noSocketAgent()

	start := time.Now()
	err := im.bootAgent(context.Background(), def)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("bootAgent must fail when the child exits before opening the socket")
	}
	if !strings.Contains(err.Error(), "process exited before opening socket") {
		t.Fatalf("expected the process-exit error, got: %v", err)
	}
	if strings.Contains(err.Error(), "did not appear within") {
		t.Fatalf("an exited child must be reported as exited, not as a socket timeout: %v", err)
	}
	if elapsed >= agentBootTimeout {
		t.Fatalf("an exited child must fail fast, took %s (timeout %s)", elapsed, agentBootTimeout)
	}
	assertNothingRegistered(t, im, def)
	if got := im.BootCount(); got != 1 {
		t.Fatalf("BootCount must count the attempt (ADR-0100 P2), got %d", got)
	}
}

// A child that stays alive but never opens its socket is given up on at
// agentBootTimeout and killed.
func TestChaos_AgentBootTimeout_SocketNeverAppears(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches a real subprocess; skipped in -short")
	}
	shortBootTimeout(t, 600*time.Millisecond)
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetEnvExtras([]string{chaosSleepEnv})
	def := noSocketAgent()

	start := time.Now()
	err := im.bootAgent(context.Background(), def)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("bootAgent must fail when the socket never appears")
	}
	if !strings.Contains(err.Error(), "did not appear within") {
		t.Fatalf("expected the boot-timeout error, got: %v", err)
	}
	if elapsed < agentBootTimeout {
		t.Fatalf("gave up before agentBootTimeout (%s < %s)", elapsed, agentBootTimeout)
	}
	if elapsed > agentBootTimeout+15*time.Second {
		t.Fatalf("took far longer than agentBootTimeout: %s", elapsed)
	}
	assertNothingRegistered(t, im, def)
	if got := im.BootCount(); got != 1 {
		t.Fatalf("BootCount must count the attempt (ADR-0100 P2), got %d", got)
	}
}

// The caller's context, not only the fixed agentBootTimeout, ends the socket poll.
func TestChaos_AgentBootTimeout_CallerCancelAbortsPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches a real subprocess; skipped in -short")
	}
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetEnvExtras([]string{chaosSleepEnv})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := im.bootAgent(ctx, noSocketAgent())
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the caller's deadline to abort the boot, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ctx cancellation was not honoured by the socket poll: took %s", elapsed)
	}
	assertNothingRegistered(t, im, noSocketAgent())
}
