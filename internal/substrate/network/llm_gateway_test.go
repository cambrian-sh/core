package network

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

func newTestGateway(concurrency int) *SubstrateLLMGateway {
	cfg := config.ExecutionConfig{
		Session: config.SessionConfig{
			SessionTokenTTLMultiplier: 5.0,
		},
		LLM: config.LLMConfig{
			LLMGatewayMaxConcurrency: concurrency,
		},
	}
	return NewLLMGateway(cfg)
}

// ─── CONWIP semaphore tests ───────────────────────────────────────────────────

func TestCONWIP_Semaphore_AllowsUpToMax(t *testing.T) {
	t.Parallel()
	const maxConcurrent = 5
	gw := newTestGateway(maxConcurrent)
	acquired := make(chan struct{}, 100)
	var wg sync.WaitGroup

	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case gw.semaphore <- struct{}{}:
				acquired <- struct{}{}
				<-gw.semaphore
			default:
			}
		}()
	}
	wg.Wait()
	close(acquired)

	count := 0
	for range acquired {
		count++
	}
	if count != maxConcurrent {
		t.Errorf("CONWIP: %d goroutines acquired semaphore, want %d", count, maxConcurrent)
	}
}

func TestCONWIP_Semaphore_BlocksBeyondMax(t *testing.T) {
	t.Parallel()
	const maxConcurrent = 3
	gw := newTestGateway(maxConcurrent)
	var mu sync.Mutex
	var blocked int

	for i := 0; i < maxConcurrent; i++ {
		gw.semaphore <- struct{}{}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case gw.semaphore <- struct{}{}:
				<-gw.semaphore
			default:
				mu.Lock()
				blocked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if blocked == 0 {
		t.Error("CONWIP: expected some goroutines to be blocked at capacity")
	}
	if blocked != 5 {
		t.Logf("CONWIP: %d goroutines blocked, up to 5 possible", blocked)
	}
}

func TestCONWIP_Semaphore_LargeScale(t *testing.T) {
	t.Parallel()
	const maxConcurrent = 20
	const totalCalls = 25

	gw := newTestGateway(maxConcurrent)
	var mu sync.Mutex
	var maxObserved int
	var currentCount int

	var wg sync.WaitGroup
	for i := 0; i < totalCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.semaphore <- struct{}{}
			mu.Lock()
			currentCount++
			if currentCount > maxObserved {
				maxObserved = currentCount
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			currentCount--
			mu.Unlock()
			<-gw.semaphore
		}()
	}
	wg.Wait()

	if maxObserved > maxConcurrent {
		t.Errorf("CONWIP: max concurrent observed = %d, want <= %d", maxObserved, maxConcurrent)
	}
	if maxObserved < 1 {
		t.Error("CONWIP: no goroutines ever acquired semaphore")
	}
}

// ─── Model chain (ADR-0139 D1) ───────────────────────────────────────────────
//
// The gateway's own health cache is gone: the Provider's breaker is the only one,
// and fallback down the chain is the Provider's (tested in internal/infrastructure/llm).
// What the gateway still owns is the ORDER of the chain it hands over.

func TestStepCandidates_WinnerThenFallbacksThenDefault(t *testing.T) {
	t.Parallel()
	sa := domain.StepAllocation{
		Winner:    domain.AgentDefinition{ID: "llm:primary"},
		Fallbacks: [2]domain.AgentDefinition{{ID: "llm:second"}, {ID: "llm:primary"}},
	}
	got := stepCandidates(sa, "llm:default")
	want := []string{"primary", "second", "default"}
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain = %v, want %v (agent ids stripped, repeats dropped)", got, want)
		}
	}
	if got := stepCandidates(domain.StepAllocation{}, ""); len(got) != 0 {
		t.Fatalf("empty allocation, no default: chain = %v, want empty", got)
	}
}

// ─── Session state store tests ───────────────────────────────────────────────

func TestSessionStore_Acquire_CreatesSession(t *testing.T) {
	t.Parallel()
	gw := newTestGateway(5)

	sa := domain.StepAllocation{
		Winner: domain.AgentDefinition{ID: "winner-agent"},
	}

	sessionID, err := gw.Acquire(context.TODO(), sa, 4096, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if sessionID == "" {
		t.Fatal("Acquire: returned empty session ID")
	}

	count := gw.SessionCount()
	if count != 1 {
		t.Errorf("SessionCount = %d, want 1", count)
	}
}

// A short estimatedDuration must NOT yield a short-lived session — the TTL is floored so
// a live step survives the slow auction + seed-recall that run before its first generate
// (otherwise EvictExpired sweeps it mid-flight → "session not found").
func TestSessionStore_Acquire_FloorsShortTTL(t *testing.T) {
	t.Parallel()
	gw := newTestGateway(5)
	sa := domain.StepAllocation{Winner: domain.AgentDefinition{ID: "w"}}

	// 30s × 5.0 = 150s, well under the floor.
	sid, err := gw.Acquire(context.TODO(), sa, 4096, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ss := gw.GetBudgetLeaseState(sid)
	if ss == nil {
		t.Fatal("session not found after Acquire")
	}
	if remaining := time.Until(ss.ExpiresAt); remaining < minSessionTTL-time.Second {
		t.Errorf("TTL must be floored at %v; got ~%v remaining", minSessionTTL, remaining)
	}
}

func TestSessionStore_Complete_RemovesSession(t *testing.T) {
	t.Parallel()
	gw := newTestGateway(5)

	sa := domain.StepAllocation{
		Winner: domain.AgentDefinition{ID: "winner"},
	}

	sid, err := gw.Acquire(context.TODO(), sa, 4096, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	usage, err := gw.Complete(context.TODO(), sid)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if usage.TotalTokens != 0 {
		t.Errorf("Complete: TotalTokens = %d, want 0 (no stream)", usage.TotalTokens)
	}

	if gw.SessionCount() != 0 {
		t.Errorf("SessionCount after Complete = %d, want 0", gw.SessionCount())
	}
}

func TestSessionStore_Complete_MissingSession(t *testing.T) {
	t.Parallel()
	gw := newTestGateway(5)
	_, err := gw.Complete(context.TODO(), "nonexistent-sess-999")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestSessionStore_EvictExpired_RemovesStale(t *testing.T) {
	t.Parallel()
	gw := newTestGateway(5)

	sa := domain.StepAllocation{
		Winner: domain.AgentDefinition{ID: "winner"},
	}

	// expired session
	expiredSS := &domain.BudgetLeaseState{
		StepAllocation: sa,
		TokenLimit:     4096,
		ExpiresAt:      time.Now().Add(-1 * time.Hour),
		LastActivityAt: time.Now().Add(-2 * time.Hour),
	}
	gw.AddSession("expired-1", expiredSS)

	// active session
	sid, err := gw.Acquire(context.TODO(), sa, 4096, time.Hour)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	gw.EvictExpired()

	count := gw.SessionCount()
	if count != 1 {
		t.Errorf("EvictExpired: %d sessions remain, want 1 (expired removed, active kept)", count)
	}

	// Verify the right one survived
	_, err = gw.Complete(context.TODO(), sid)
	if err != nil {
		t.Errorf("active session should have survived: %v", err)
	}
}
