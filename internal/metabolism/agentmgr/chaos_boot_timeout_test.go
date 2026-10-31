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
// its UDS socket (T-20261031-documented-production-failure-modes-have-no).
// Guards instance_manager.go bootAgent, Phase 1:
//
//	if time.Now().After(deadline) { _ = cmd.Process.Kill(); return fmt.Errorf("… did not appear within %s") }
//	case <-ctx.Done(): _ = cmd.Process.Kill(); return ctx.Err()
//
// The child is this test binary launched as a RuntimeBinary agent. bootAgent
// hands it `--socket … --substrate-addr …`, which the Go test flag parser
// rejects, so the child exits at once and never creates the socket — the same
// observable as an agent that hangs in import. (A child that lingers would be a
// better test of the Kill; see the ticket filed from this test about bootAgent
// not noticing an early exit.)

func noSocketAgent() *domain.AgentDefinition {
	return &domain.AgentDefinition{
		ID:       "chaos-never-opens-socket",
		Runtime:  domain.RuntimeBinary,
		ExecPath: os.Args[0],
		Trait:    domain.TraitCognitive,
	}
}

func TestChaos_AgentBootTimeout_SocketNeverAppears(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches a real subprocess and waits the full bootTimeout; skipped in -short")
	}
	const bootTimeout = 10 * time.Second // mirrors the const in bootAgent

	im := NewInstanceManager("", "127.0.0.1:1")
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
	if elapsed < bootTimeout {
		t.Fatalf("gave up before bootTimeout (%s < %s)", elapsed, bootTimeout)
	}
	if elapsed > bootTimeout+15*time.Second {
		t.Fatalf("took far longer than bootTimeout: %s", elapsed)
	}

	im.mu.Lock()
	defer im.mu.Unlock()
	if n := len(im.instances); n != 0 {
		t.Fatalf("a failed boot must not register an instance, found %d", n)
	}
	if ids := im.agentIndex[def.ID]; len(ids) != 0 {
		t.Fatalf("a failed boot must not index the agent, found %v", ids)
	}
	if got := im.BootCount(); got != 1 {
		t.Fatalf("BootCount must count the attempt (ADR-0100 P2), got %d", got)
	}
}

// The caller's context, not only the fixed bootTimeout, ends the socket poll.
func TestChaos_AgentBootTimeout_CallerCancelAbortsPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test launches a real subprocess; skipped in -short")
	}
	im := NewInstanceManager("", "127.0.0.1:1")
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
	im.mu.Lock()
	defer im.mu.Unlock()
	if n := len(im.instances); n != 0 {
		t.Fatalf("an aborted boot must not register an instance, found %d", n)
	}
}
