package signal_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/supervision/signal"

	"google.golang.org/grpc/metadata"
)

// ── CircuitBreaker ────────────────────────────────────────────────────────────

// Cycle 1 — RecordInvalidSignal increments the count for an agent.
func TestCircuitBreaker_TracksInvalidSignals(t *testing.T) {
	cb := signal.NewCircuitBreaker(3, 10)

	cb.RecordInvalidSignal("agent-1")
	cb.RecordInvalidSignal("agent-1")

	if got := cb.Count("agent-1"); got != 2 {
		t.Errorf("Count: want 2, got %d", got)
	}
}

// Cycle 2 — ShouldInhibit returns false below threshold, true at threshold.
func TestCircuitBreaker_ShouldInhibit_TripsAtThreshold(t *testing.T) {
	cb := signal.NewCircuitBreaker(2, 10)

	cb.RecordInvalidSignal("agent-x")
	if cb.ShouldInhibit("agent-x") {
		t.Error("ShouldInhibit: want false after 1 signal, threshold is 2")
	}

	cb.RecordInvalidSignal("agent-x")
	if !cb.ShouldInhibit("agent-x") {
		t.Error("ShouldInhibit: want true after 2 signals, threshold is 2")
	}
}

// Cycle 3 — ResetInvalidSignals clears the counter to zero.
func TestCircuitBreaker_ResetClearsCount(t *testing.T) {
	cb := signal.NewCircuitBreaker(3, 10)

	cb.RecordInvalidSignal("agent-1")
	cb.RecordInvalidSignal("agent-1")
	cb.ResetInvalidSignals("agent-1")

	if got := cb.Count("agent-1"); got != 0 {
		t.Errorf("Count after reset: want 0, got %d", got)
	}
}

// Cycle 4 — Signals older than the window are not counted.
func TestCircuitBreaker_SlidingWindowExpiresOldSignals(t *testing.T) {
	cb := signal.NewCircuitBreaker(3, 1)

	cb.RecordAt("agent-1", time.Now().Add(-2*time.Second))

	if got := cb.Count("agent-1"); got != 0 {
		t.Errorf("Count after window: want 0 (expired), got %d", got)
	}
}

// ── GenerateAuthToken ─────────────────────────────────────────────────────────

// Cycle 5 — GenerateAuthToken returns a non-empty hex string.
func TestGenerateAuthToken_NonEmpty(t *testing.T) {
	tok, err := signal.GenerateAuthToken()
	if err != nil {
		t.Fatalf("GenerateAuthToken: %v", err)
	}
	if len(tok) < 16 {
		t.Errorf("token too short: %q (len %d)", tok, len(tok))
	}
}

// Cycle 6 — Two calls return different tokens (uniqueness).
func TestGenerateAuthToken_Unique(t *testing.T) {
	a, _ := signal.GenerateAuthToken()
	b, _ := signal.GenerateAuthToken()
	if a == b {
		t.Error("GenerateAuthToken: two tokens should not be equal")
	}
}

// ── ValidateToken ─────────────────────────────────────────────────────────────

type noopManager struct{}

func (noopManager) FindInstanceByToken(_ string) *domain.Instance { return nil }
func (noopManager) GetInstanceIDs(_ string) []string              { return nil }
func (noopManager) EvictInstance(_ string)                        {}

// Cycle 7 — ValidateToken errors when gRPC metadata is absent.
func TestValidateToken_MissingMetadata(t *testing.T) {
	_, err := signal.ValidateToken(context.Background(), noopManager{})
	if err == nil {
		t.Error("expected error for missing gRPC metadata")
	}
}

// Cycle 8 — ValidateToken errors when authorization header is empty.
func TestValidateToken_EmptyAuthHeader(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", ""))
	_, err := signal.ValidateToken(ctx, noopManager{})
	if err == nil {
		t.Error("expected error for empty authorization header")
	}
}

// Cycle 9 — ValidateToken errors when authorization is not Bearer format.
func TestValidateToken_InvalidBearerFormat(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic abc123"))
	_, err := signal.ValidateToken(ctx, noopManager{})
	if err == nil {
		t.Error("expected error for non-Bearer authorization")
	}
}

// Cycle 10 — ValidateToken errors when the token is unknown (manager returns nil).
func TestValidateToken_UnknownToken(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer unknown-token"))
	_, err := signal.ValidateToken(ctx, noopManager{})
	if err == nil {
		t.Error("expected error for unknown bearer token")
	}
}

// Cycle 11 — ValidateToken returns the instance when the token is recognised.
func TestValidateToken_ValidToken(t *testing.T) {
	inst := &domain.Instance{AgentID: "agent-99", ID: "inst-99"}
	mgr := &stubManager{inst: inst, token: "valid-tok"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer valid-tok"))

	got, err := signal.ValidateToken(ctx, mgr)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if got.AgentID != "agent-99" {
		t.Errorf("AgentID: want %q, got %q", "agent-99", got.AgentID)
	}
}

type stubManager struct {
	inst  *domain.Instance
	token string
}

func (s *stubManager) FindInstanceByToken(tok string) *domain.Instance {
	if tok == s.token {
		return s.inst
	}
	return nil
}
func (s *stubManager) GetInstanceIDs(_ string) []string { return nil }
func (s *stubManager) EvictInstance(_ string)           {}
