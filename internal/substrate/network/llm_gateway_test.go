package network

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
)

func newTestGateway(concurrency int) *SubstrateLLMGateway {
	cfg := config.ExecutionConfig{
		LLMGatewayMaxConcurrency:  concurrency,
		SessionTokenTTLMultiplier: 5.0,
	}
	return NewLLMGateway(cfg)
}

// ─── CONWIP semaphore tests ───────────────────────────────────────────────────

func TestCONWIP_Semaphore_AllowsUpToMax(t *testing.T) {
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

// ─── Health cache fallback tests ─────────────────────────────────────────────

func TestHealthCache_PrimaryUnhealthyRoutesToFallback(t *testing.T) {
	gw := newTestGateway(5)

	modelIDs := []string{"primary-model", "fallback-model"}

	selected, err := gw.selectHealthyModel(modelIDs)
	if err != nil {
		t.Fatalf("selectHealthyModel returned error: %v", err)
	}
	if selected != "primary-model" {
		t.Errorf("healthy primary: got %q, want %q", selected, "primary-model")
	}

	gw.MarkUnhealthy("primary-model")

	selected, err = gw.selectHealthyModel(modelIDs)
	if err != nil {
		t.Fatalf("selectHealthyModel returned error: %v", err)
	}
	if selected != "fallback-model" {
		t.Errorf("unhealthy primary: got %q, want %q (fallback)", selected, "fallback-model")
	}
}

func TestHealthCache_AllCandidatesDegradedReturnsError(t *testing.T) {
	gw := newTestGateway(5)

	modelIDs := []string{"m1", "m2"}
	gw.MarkUnhealthy("m1")
	gw.MarkUnhealthy("m2")

	_, err := gw.selectHealthyModel(modelIDs)
	if err == nil {
		t.Fatal("expected error when all models degraded")
	}
}

func TestHealthCache_HealthStateReflectsMarking(t *testing.T) {
	gw := newTestGateway(5)

	state := gw.HealthState("new-model")
	if state != healthHealthy {
		t.Errorf("unmarked model: state = %v, want healthy", state)
	}

	gw.MarkUnhealthy("new-model")
	state = gw.HealthState("new-model")
	if state != healthUnhealthy {
		t.Errorf("marked model: state = %v, want unhealthy", state)
	}
}

// ─── Session state store tests ───────────────────────────────────────────────

func TestSessionStore_Acquire_CreatesSession(t *testing.T) {
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
	gw := newTestGateway(5)
	sa := domain.StepAllocation{Winner: domain.AgentDefinition{ID: "w"}}

	// 30s × 5.0 = 150s, well under the floor.
	sid, err := gw.Acquire(context.TODO(), sa, 4096, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ss := gw.GetSessionState(sid)
	if ss == nil {
		t.Fatal("session not found after Acquire")
	}
	if remaining := time.Until(ss.ExpiresAt); remaining < minSessionTTL-time.Second {
		t.Errorf("TTL must be floored at %v; got ~%v remaining", minSessionTTL, remaining)
	}
}

func TestSessionStore_Complete_RemovesSession(t *testing.T) {
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
	gw := newTestGateway(5)
	_, err := gw.Complete(context.TODO(), "nonexistent-sess-999")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestSessionStore_EvictExpired_RemovesStale(t *testing.T) {
	gw := newTestGateway(5)

	sa := domain.StepAllocation{
		Winner: domain.AgentDefinition{ID: "winner"},
	}

	// expired session
	expiredSS := &domain.SessionState{
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
