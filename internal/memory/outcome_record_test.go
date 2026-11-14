package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeExperienceStore captures the episode parent rows the agent mints.
type fakeExperienceStore struct {
	saved []domain.Experience
	err   error
}

func (f *fakeExperienceStore) LinkDerivation(_ context.Context, _ string, _ []string) error {
	return nil
}

func (f *fakeExperienceStore) SaveExperience(_ context.Context, e domain.Experience) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, e)
	return nil
}

// TestBornTags_NeverEmpty is ADR-0095 D4's first clause as a test: there is no input
// that yields an untagged experience. An untagged row has no tags for any predicate to
// act on, which is how a boundary becomes inexpressible rather than merely open.
func TestBornTags_NeverEmpty(t *testing.T) {
	t.Parallel()
	for _, surface := range []domain.SurfaceRef{
		{},
		{Kind: domain.SurfaceChat},
		{Kind: domain.SurfaceChat, ID: "airline"},
		{ID: "airline"},
	} {
		tags := domain.BornTags(surface)
		if len(tags) == 0 {
			t.Fatalf("surface %+v produced no tags", surface)
		}
		var hasInternal bool
		for _, tag := range tags {
			if tag == domain.TagInternal {
				hasInternal = true
			}
		}
		if !hasInternal {
			t.Errorf("surface %+v: expected %q among %v", surface, domain.TagInternal, tags)
		}
	}
}

// The surface must reach the tags, or a customer-surface episode is indistinguishable
// from an internal one and ADR-0087's RequiredTags clamp has nothing to clamp on.
func TestBornTags_CarriesSurfaceAndIngress(t *testing.T) {
	t.Parallel()
	tags := domain.BornTags(domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "airline"})
	want := map[string]bool{"internal": false, "surface:chat": false, "ingress:airline": false}
	for _, tag := range tags {
		if _, ok := want[tag]; ok {
			want[tag] = true
		}
	}
	for tag, seen := range want {
		if !seen {
			t.Errorf("missing tag %q in %v", tag, tags)
		}
	}
}

// The arm is default-off, and off must mean NOTHING is written — not "written but
// filtered later". This is the post-2026-07-18 baseline and the control arm of every
// A/B that measures the outcome record.
func TestRecordPlanOutcome_DefaultOffWritesNothing(t *testing.T) {
	t.Parallel()
	store := &fakeExperienceStore{}
	agent := &Agent{ExperienceStore: store} // RecordOutcomes defaults false
	if err := agent.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "plan-1", Goal: "do a thing", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("arm is off; no experience should be minted, got %+v", store.saved)
	}
}

// A nil Manager must not panic — the gate is checked before any dereference.
func TestRecordPlanOutcome_NilManagerIsSafe(t *testing.T) {
	t.Parallel()
	agent := &Agent{RecordOutcomes: true}
	if err := agent.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "plan-1", Goal: "goal", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An empty plan id is not an episode.
func TestRecordPlanOutcome_EmptyPlanIDWritesNothing(t *testing.T) {
	t.Parallel()
	store := &fakeExperienceStore{}
	agent := &Agent{RecordOutcomes: true, ExperienceStore: store}
	if err := agent.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "", Goal: "goal", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("empty plan id must mint nothing, got %+v", store.saved)
	}
}

// multiCaptureStore keeps EVERY saved doc. The shared captureSaveStore keeps only the
// last, which cannot answer how many rows a plan wrote.
type multiCaptureStore struct {
	fakeVectorStore
	saved []domain.Document
}

func (m *multiCaptureStore) Save(_ context.Context, doc *domain.Document) error {
	m.saved = append(m.saved, *doc)
	return nil
}

// ENG-1595 (owner decision 2026-11-14): a failed plan — surprising or not, replan-
// exhausted or not — writes NO failure record (negative_edge chunk) any more; only its
// episode row records the failure.
func TestRecordPlanOutcome_FailureWritesNoNegativeEdge(t *testing.T) {
	t.Parallel()
	store := &multiCaptureStore{}
	a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 3)
	a.RecordOutcomes = true
	for _, rec := range []domain.PlanRecord{
		{PlanID: "p-surprise", Goal: "migrate", Success: false, Surprise: 0.9, FailureSummary: "step 1: boom"},
		{PlanID: "p-exhausted", Goal: "migrate", Success: false, Surprise: -1, ReplanExhausted: true, FailureSummary: "gave up"},
	} {
		if err := a.RecordPlanOutcome(context.Background(), rec); err != nil {
			t.Fatalf("RecordPlanOutcome: %v", err)
		}
	}
	if len(store.saved) != 0 {
		t.Fatalf("no chunk may be saved for a failed plan, got %+v", store.saved)
	}
}

// decision (2026-11-05, scenes cancelled): procedure feedback used to sit BELOW the
// "contentless scene" early return, so a plan that engaged no entity never fed its
// procedures. It now runs for every plan (the failure record beside it was stopped by
// ENG-1595).
func TestRecordPlanOutcome_NoEngagementStillFeedsProcedures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newLifecycleStore()
	routine := domain.Procedure{
		ID: "proc-r", Trigger: "reason about it", Steps: []domain.ProcedureStep{{Intent: "think"}},
		Confidence: 0.6, Status: domain.ProcedureActive,
	}
	if err := SaveProcedure(ctx, store, &recordingEmbedder{}, routine); err != nil {
		t.Fatalf("seed procedure: %v", err)
	}
	a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 3)
	a.RecordOutcomes = true
	a.ProcedureDeprecateBelow = 0.3

	// No RecordToolOutput call: the plan engaged nothing.
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{
		PlanID: "p-pure", Goal: "reason about it", Success: false, Surprise: -1,
		ReplanExhausted: true, FailureSummary: "step 0 (think): gave up",
		FollowedProcedures: []string{"proc-r"},
	})

	if edges := store.ofType(domain.DocTypeNegativeEdge); len(edges) != 0 {
		t.Fatalf("no failure record may be written (ENG-1595), got %d", len(edges))
	}
	got, ok := procedureFromDoc(store.docs["proc-r"])
	if !ok {
		t.Fatal("procedure row unreadable after feedback")
	}
	if !(got.Confidence < routine.Confidence) {
		t.Errorf("the failed plan must feed its procedure even with no engaged entity: confidence %v → %v",
			routine.Confidence, got.Confidence)
	}
}
