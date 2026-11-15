package memory

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Procedure feedback reads its own store by id on the plan-completion path. ADR-0095 D9
// made by-identity reads ENFORCED, and no principal is asking on that path, so it must
// seed the explicit domain.ScopeSystem bypass or fail closed.
//
// Failing closed is not visible: the reader treats an error as "nothing found", which is
// indistinguishable from a routine that was never stored. So this test runs the REAL
// chokepoint (authz.EnforcingVectorStore, the decorator the kernel actually wires) over
// the lifecycle store. (The scene dedup probe and the scene action-path reads this file
// also covered were deleted with scenes on 2026-11-05.)

// strictScopeStore wraps a lifecycle store in the production read chokepoint, so an
// unseeded by-identity read returns authz.ErrScopeMissing exactly as it does live.
// Writes go straight to the lifecycle store: the read tier has no write side
// (ENG-1476), and the kernel composes the two tiers the same way.
func strictScopeStore(inner domain.VectorStore) domain.VectorReadWriter {
	return struct {
		domain.VectorReader
		domain.VectorWriter
	}{authz.NewEnforcingVectorStore(inner, slog.New(slog.NewTextHandler(io.Discard, nil))), inner}
}

// TestFeedProcedureOutcome_SurvivesEnforcedReads closes the ADR-0094 co-evolution loop
// through the chokepoint.
//
// Remove the domain.ScopeSystem seed in FeedProcedureOutcome and this fails: GetByID
// returns ErrScopeMissing, every routine reads as "not found", and confidence never moves
// — the tier keeps shaping plans and stops learning from them.
func TestFeedProcedureOutcome_SurvivesEnforcedReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newLifecycleStore()
	enforced := strictScopeStore(store)
	emb := &recordingEmbedder{}

	proc := domain.Procedure{
		ID:          "proc-enforced",
		Trigger:     "ship the billing service",
		Steps:       []domain.ProcedureStep{{Intent: "build", RequiredCapabilities: []string{"build"}}},
		SampleCount: 3,
		Confidence:  0.5,
		Status:      domain.ProcedureActive,
	}
	if err := SaveProcedure(ctx, enforced, emb, proc); err != nil {
		t.Fatalf("fixture save failed: %v", err)
	}

	FeedProcedureOutcome(ctx, enforced, emb, []string{proc.ID}, true, 0.3)

	after, ok := procedureFromDoc(store.docs[proc.ID])
	if !ok {
		t.Fatalf("routine vanished from the store")
	}
	if after.SampleCount != proc.SampleCount+1 {
		t.Errorf("following a routine must be counted against it: samples %d → %d (read failed closed)",
			proc.SampleCount, after.SampleCount)
	}
	if after.Confidence <= proc.Confidence {
		t.Errorf("a success must raise confidence: %.3f → %.3f", proc.Confidence, after.Confidence)
	}
}
