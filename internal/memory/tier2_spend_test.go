package memory

import (
	"context"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// sessionCapturingGenerator records the spend attribution of each Tier-2 call.
type sessionCapturingGenerator struct {
	mu    sync.Mutex
	calls []struct {
		sid        domain.SessionID
		background bool
	}
}

func (g *sessionCapturingGenerator) Generate(ctx context.Context, _ string) (string, error) {
	sid, _ := domain.SessionIDFromContext(ctx)
	g.mu.Lock()
	g.calls = append(g.calls, struct {
		sid        domain.SessionID
		background bool
	}{sid, domain.IsBackgroundSpend(ctx)})
	g.mu.Unlock()
	return `[]`, nil
}

// (b) ADR-0131, owner decision 2026-11-01: the Tier-2 judge runs once per
// originating session, under that session and the background-spend marker, so
// its spend is booked to the session and never refused at its ceiling; items
// with no session are judged under no session (the kernel no-session bucket).
func TestTier2_ScoresPerOriginatingSessionAsBackgroundSpend(t *testing.T) {
	t.Parallel()
	gen := &sessionCapturingGenerator{}
	agent := testAgentWithMocks(gen)
	for i, sid := range []domain.SessionID{"ses-1", "ses-2", "ses-1", ""} {
		agent.pendingItems = append(agent.pendingItems, pendingItem{
			Embedding: []float32{float32(i + 1), 0, 0},
			Doc:       &domain.Document{ID: string(rune('a' + i)), Text: "result", Metadata: map[string]interface{}{"step_index": i}},
			SessionID: sid,
		})
	}
	agent.drainBatch(context.Background())

	gen.mu.Lock()
	defer gen.mu.Unlock()
	if len(gen.calls) != 3 {
		t.Fatalf("want one judge call per originating session (3), got %d: %+v", len(gen.calls), gen.calls)
	}
	want := []domain.SessionID{"ses-1", "ses-2", ""}
	for i, c := range gen.calls {
		if c.sid != want[i] || !c.background {
			t.Fatalf("call %d = %+v, want session %q with the background marker", i, c, want[i])
		}
	}
}
