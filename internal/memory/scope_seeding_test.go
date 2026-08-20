package memory

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// The experiential write path reads its own store — dedup probes it by scene_identity,
// the scene's inline action summary resolves it by plan_id, and procedure feedback fetches
// routines by id. ADR-0095 D9 made those by-identity reads ENFORCED, and no principal is
// asking on that path, so every one of them must seed the explicit domain.ScopeSystem
// bypass or fail closed.
//
// Failing closed is not visible: each of those readers treats an error as "nothing found",
// which is indistinguishable from a genuinely new situation, a plan with no actions, and a
// routine that was never stored. So these tests run the REAL chokepoint
// (authz.EnforcingVectorStore, the decorator the kernel actually wires) over the lifecycle
// store, and assert the OUTCOMES that fail-closed silently erases.

// strictScopeStore wraps a lifecycle store in the production read chokepoint, so an
// unseeded by-identity read returns authz.ErrScopeMissing exactly as it does live.
func strictScopeStore(inner domain.VectorStore) domain.VectorStore {
	return authz.NewEnforcingVectorStore(inner, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// strictScopeAgent is lifecycleAgent with the read chokepoint interposed between the
// memory agent and its store. Everything else — arms, floors, embedder — is identical, so
// a difference against the lifecycle tests is a scope-seeding difference and nothing else.
func strictScopeAgent(store *lifecycleStore) *Agent {
	a := NewAgent(NewMemoryManager(strictScopeStore(store), &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
	a.RecordOutcomes = true
	a.SurpriseFloor = 0.5
	a.ProcedureDeprecateBelow = 0.3
	a.ExperienceStore = store
	return a
}

// TestSceneDedup_SurvivesEnforcedReads is TestSceneDedup_RerunReinforcesRatherThanDuplicates
// run through the enforcing read chokepoint.
//
// Remove the domain.ScopeSystem seed in findSceneByProjection and this fails: the dedup
// probe gets ErrScopeMissing, every rerun reads as an unseen situation, and the second
// occurrence inserts a sibling scene instead of reinforcing the first — restoring exactly
// the row inflation A2.9 exists to prevent, where induction counts reruns as
// confirmations.
func TestSceneDedup_SurvivesEnforcedReads(t *testing.T) {
	store := newLifecycleStore()
	agent := strictScopeAgent(store)

	rerun := mockPlan{
		id: "e1", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
		touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.1,
	}
	for _, id := range []string{"e1", "e2"} {
		p := rerun
		p.id = id
		runPlan(t, agent, p)
	}

	scenes := store.ofType(domain.DocTypeMnemonicScene)
	if len(scenes) != 1 {
		t.Fatalf("one situation lived twice must leave ONE scene, got %d (dedup read failed closed)", len(scenes))
	}
	if seen, _ := scenes[0].Metadata["seen_count"].(int); seen != 2 {
		t.Errorf("the reinforced scene must count both occurrences, got seen_count=%v", scenes[0].Metadata["seen_count"])
	}
	// Both plans are still their own episode: dedup applies to the RECORD, not the event.
	if len(store.experiences) != 2 {
		t.Errorf("expected one episode per plan, got %d", len(store.experiences))
	}
}

// TestPlanScene_CarriesActionsUnderEnforcedReads pins the scene's inline action summary.
//
// Remove the domain.ScopeSystem seed in resolveActionPath and this fails: planActionLines
// gets ErrScopeMissing, the scene is written with an empty "actions" list, and the record
// degrades to a situational index with no "what I did" — silently, because a plan that
// engaged nothing produces the same shape.
func TestPlanScene_CarriesActionsUnderEnforcedReads(t *testing.T) {
	store := newLifecycleStore()
	agent := strictScopeAgent(store)

	runPlan(t, agent, mockPlan{
		id: "ea1", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
		touches: []string{"/srv/billing/main.go", "/srv/billing/api.go"}, success: true, surprise: 0.1,
	})

	scenes := store.ofType(domain.DocTypeMnemonicScene)
	if len(scenes) != 1 {
		t.Fatalf("expected exactly 1 scene, got %d", len(scenes))
	}
	actions, _ := scenes[0].Metadata["actions"].([]string)
	if len(actions) != 2 {
		t.Fatalf("the scene must inline the plan's action path, got %d lines: %v (action read failed closed)",
			len(actions), scenes[0].Metadata["actions"])
	}
}

// TestRetrievePrecedents_CarriesActionsUnderEnforcedReads covers the planner push lane
// (workspace_stage.retrievePrecedentLane) and the agent pull lane
// (QueryService.SearchPrecedents), both of which reach resolveActionPath on a ctx nobody
// seeded — the surrounding Search passes SearchOptions.Scope, which QueryByMetadata has no
// equivalent of.
//
// Remove the domain.ScopeSystem seed in resolveActionPath and this fails: every precedent
// renders situation + outcome with no path, so the LLM is told what happened last time and
// never what was done.
func TestRetrievePrecedents_CarriesActionsUnderEnforcedReads(t *testing.T) {
	store := newLifecycleStore()
	agent := strictScopeAgent(store)

	runPlan(t, agent, mockPlan{
		id: "ep1", goal: "migrate the legacy exporter", capabilities: []string{"migrate"},
		touches: []string{"/srv/legacy/export.go"}, success: false, surprise: 0.1,
	})

	scene, ok := store.docs["scene-ep1"]
	if !ok {
		t.Fatalf("fixture did not write the scene; store holds %d docs", len(store.docs))
	}
	precedents := retrievePrecedents(context.Background(), strictScopeStore(store),
		[]domain.SearchResult{{Document: scene, Score: 0.9}})

	if len(precedents) != 1 {
		t.Fatalf("expected 1 precedent, got %d", len(precedents))
	}
	if len(precedents[0].Actions) == 0 {
		t.Error("a precedent must carry the action path of the scene it was built from; " +
			"an empty path means the by-plan_id read failed closed")
	}
	if precedents[0].Outcome != "failure" {
		t.Errorf("outcome must come from the scene metadata, got %q", precedents[0].Outcome)
	}
}

// TestFeedProcedureOutcome_SurvivesEnforcedReads closes the ADR-0094 co-evolution loop
// through the chokepoint.
//
// Remove the domain.ScopeSystem seed in FeedProcedureOutcome and this fails: GetByID
// returns ErrScopeMissing, every routine reads as "not found", and confidence never moves
// — the tier keeps shaping plans and stops learning from them.
func TestFeedProcedureOutcome_SurvivesEnforcedReads(t *testing.T) {
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
