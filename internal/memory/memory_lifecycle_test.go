package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// End-to-end behaviour of the experiential memory loop, driven by MOCK PLAN DATA.
//
// It runs plausible planner output through the REAL write path (action records, the
// plan-outcome hook) and the REAL procedure feedback cycle, and asserts what the system
// ends up remembering. Scenes, scene dedup and procedure induction were removed on
// 2026-11-05 (decisions/scenes-cancelled.md); stored procedures are still served and
// still learn from outcomes.
//
// It is deliberately narrative. Run it with -v and it reads as a description of how the
// system memorises, which is the thing a reader actually wants to know.

// ---------------------------------------------------------------------------
// A store that behaves enough like pgvector for the loop to run.
// ---------------------------------------------------------------------------

type lifecycleStore struct {
	fakeVectorStore
	docs map[string]domain.Document
	// experiences records what the episode-parent writer was asked to persist.
	experiences map[string]domain.Experience
	links       map[string][]string
}

func newLifecycleStore() *lifecycleStore {
	return &lifecycleStore{
		docs:        map[string]domain.Document{},
		experiences: map[string]domain.Experience{},
		links:       map[string][]string{},
	}
}

func (l *lifecycleStore) Save(_ context.Context, doc *domain.Document) error {
	l.docs[doc.ID] = *doc
	return nil
}

func (l *lifecycleStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := l.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

// QueryByMetadata uses containment semantics, matching the adapter's `@>`.
func (l *lifecycleStore) QueryByMetadata(_ context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	var out []domain.Document
	for _, d := range l.docs {
		match := true
		for k, v := range filter {
			if got, _ := d.Metadata[k].(string); got != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, d)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (l *lifecycleStore) SaveExperience(_ context.Context, e domain.Experience) error {
	l.experiences[e.ID] = e
	return nil
}

func (l *lifecycleStore) LinkDerivation(_ context.Context, derived string, exps []string) error {
	l.links[derived] = append(l.links[derived], exps...)
	return nil
}

func (l *lifecycleStore) countType(t string) int {
	n := 0
	for _, d := range l.docs {
		if d.DocumentType == t {
			n++
		}
	}
	return n
}

func (l *lifecycleStore) ofType(t string) []domain.Document {
	var out []domain.Document
	for _, d := range l.docs {
		if d.DocumentType == t {
			out = append(out, d)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Mock planner output
// ---------------------------------------------------------------------------

// mockPlan is what a planner would have produced, reduced to what memory sees.
type mockPlan struct {
	id       string
	goal     string
	touches  []string // file paths the plan's tools engaged
	success  bool
	surprise float64 // |merit expectation - actual|, as A2.3 computes it
	// followedProcedures are the ADR-0094 routines that informed this plan — the
	// provenance that closes the co-evolution loop (D8).
	followedProcedures []string
}

// runPlan drives one mock plan through the REAL write path: each touch is a mutation
// (an action record), then the plan completes through the plan-outcome hook.
func runPlan(t *testing.T, a *Agent, p mockPlan) {
	t.Helper()
	ctx := context.Background()
	for i, path := range p.touches {
		_ = a.RecordToolOutput(ctx, domain.ToolOutputRecord{
			ToolName:   "write_file",
			ArgsJSON:   []byte(fmt.Sprintf(`{"path":%q}`, path)),
			Output:     []byte(`{"ok":1}`),
			IsMutation: true,
			TaskID:     fmt.Sprintf("step-%d-%s", i, p.id),
		})
	}
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{
		PlanID:             p.id,
		Goal:               p.goal,
		Success:            p.success,
		Surprise:           p.surprise,
		FollowedProcedures: p.followedProcedures,
	})
}

func lifecycleAgent(store *lifecycleStore) *Agent {
	return lifecycleAgentWith(store, &recordingEmbedder{})
}

func lifecycleAgentWith(store *lifecycleStore, emb domain.Embedder) *Agent {
	a := NewAgent(NewMemoryManager(store, emb), 0.7, 5, 3)
	a.RecordOutcomes = true // ADR-0049 A2.2 arm ON for this test
	a.SurpriseFloor = 0.5   // A2.3 failure-precedent gate
	a.ProcedureDeprecateBelow = 0.3
	a.ExperienceStore = store
	return a
}

// ---------------------------------------------------------------------------
// The scenario
// ---------------------------------------------------------------------------

// TestMemoryLifecycle_WhatTheSystemRemembers runs a plausible week of planner activity
// and asserts what survives in memory.
func TestMemoryLifecycle_WhatTheSystemRemembers(t *testing.T) {
	t.Parallel()
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	// Three occurrences of the SAME routine (build then deploy), plus unrelated work,
	// plus two failures: one expected, one shocking.
	plans := []mockPlan{
		{id: "p1", goal: "ship the billing service", touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.1},
		{id: "p2", goal: "ship the billing service", touches: []string{"/srv/billing/api.go"}, success: true, surprise: 0.05},
		{id: "p3", goal: "ship the billing service", touches: []string{"/srv/billing/db.go"}, success: true, surprise: 0.2},
		{id: "p4", goal: "summarise the quarterly report", touches: []string{"/docs/q3.md"}, success: true, surprise: 0.1},
		// A failure nobody is surprised by: merit already expected this to go badly.
		{id: "p5", goal: "migrate the legacy exporter", touches: []string{"/srv/legacy/export.go"}, success: false, surprise: 0.1},
		// A failure that contradicts a confident expectation — the informative one.
		{id: "p6", goal: "ship the billing service", touches: []string{"/srv/billing/cache.go"}, success: false, surprise: 0.9},
	}
	for _, p := range plans {
		runPlan(t, agent, p)
	}

	t.Log("--- after 6 plans ---")
	t.Logf("episodes (experience parents): %d", len(store.experiences))
	t.Logf("action records:                %d", store.countType(domain.DocTypeMnemonicAction))
	t.Logf("failure precedents:            %d", store.countType(domain.DocTypeNegativeEdge))

	// Every plan becomes exactly one EPISODE, and every touch one action record.
	if len(store.experiences) != len(plans) {
		t.Errorf("expected one episode per plan, got %d", len(store.experiences))
	}
	if got := store.countType(domain.DocTypeMnemonicAction); got != len(plans) {
		t.Errorf("expected one action record per touch (%d), got %d", len(plans), got)
	}

	// A2.3: only the SURPRISING failure earns a precedent. The expected one does not —
	// that is the difference between a surprise gate and a failure gate.
	if got := store.countType(domain.DocTypeNegativeEdge); got != 1 {
		t.Errorf("expected exactly 1 failure precedent (the surprising one), got %d", got)
	}

	// ADR-0095 D4: every episode is born tagged, and none can be born untagged.
	for id, e := range store.experiences {
		if len(e.Tags) == 0 {
			t.Errorf("episode %s was born untagged — the boundary would be inexpressible", id)
		}
		var internal bool
		for _, tag := range e.Tags {
			if tag == domain.TagInternal {
				internal = true
			}
		}
		if !internal {
			t.Errorf("episode %s missing the internal classification: %v", id, e.Tags)
		}
	}

	// The plan-level outcome chunk (`mnemonic_scene`) is gone: what a plan leaves is
	// its episode row, its action records and, for a surprising failure, a failure
	// record. Nothing else.
	if got := len(store.docs); got != len(plans)+1 {
		t.Errorf("expected %d chunks (actions + one failure record), got %d", len(plans)+1, got)
	}
}

// TestMemoryLifecycle_RefinesAStoredRoutine: a stored procedure learns from how
// following it goes, and retires when it stops working. (It used to be induced from
// scenes first; induction was removed 2026-11-05, so the routine is seeded.)
func TestMemoryLifecycle_RefinesAStoredRoutine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newLifecycleStore()
	p := domain.Procedure{
		ID:          "proc-billing",
		Trigger:     "ship the billing service",
		Steps:       []domain.ProcedureStep{{RequiredCapabilities: []string{"build"}, Intent: "perform: build"}},
		Confidence:  0.6,
		SampleCount: 3,
		Status:      domain.ProcedureActive,
	}
	if err := SaveProcedure(ctx, store, &recordingEmbedder{}, p); err != nil {
		t.Fatalf("seed routine: %v", err)
	}

	emb := &recordingEmbedder{}
	FeedProcedureOutcome(ctx, store, emb, []string{p.ID}, true, 0.3)
	after, _ := procedureFromDoc(store.docs[p.ID])
	t.Logf("after one success:  confidence=%.3f samples=%d status=%s",
		after.Confidence, after.SampleCount, after.Status)
	if after.SampleCount <= p.SampleCount {
		t.Error("following a routine must be counted against it")
	}

	for i := 0; i < 30; i++ {
		FeedProcedureOutcome(ctx, store, emb, []string{p.ID}, false, 0.3)
	}
	final, _ := procedureFromDoc(store.docs[p.ID])
	t.Logf("after 30 failures:  confidence=%.3f samples=%d status=%s",
		final.Confidence, final.SampleCount, final.Status)
	if final.Status != domain.ProcedureDeprecated {
		t.Errorf("a routine that stops working must retire, got %s", final.Status)
	}
	// Retired, not erased: the record survives as evidence.
	if _, still := store.docs[p.ID]; !still {
		t.Error("a deprecated routine must remain as evidence, not be deleted")
	}
}

// TestMemoryLifecycle_ArmOffRemembersNothing is the control. With the default
// configuration the entire loop must be inert — this is the post-2026-07-18 baseline and
// the arm every A/B measures against.
func TestMemoryLifecycle_ArmOffRemembersNothing(t *testing.T) {
	t.Parallel()
	store := newLifecycleStore()
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.7, 5, 3)
	agent.ExperienceStore = store // wired, but both arms default false

	runPlan(t, agent, mockPlan{
		id: "p1", goal: "ship the billing service", touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.9,
	})

	t.Logf("arm off — episodes:%d actions:%d",
		len(store.experiences),
		store.countType(domain.DocTypeMnemonicAction))

	if len(store.experiences) != 0 || len(store.docs) != 0 {
		t.Errorf("with the arm off the system must remember NOTHING, got %d episodes / %d docs",
			len(store.experiences), len(store.docs))
	}
}

// TestProcedureFeedbackLoop_ClosesEndToEnd is the ADR-0094 D8 co-evolution loop.
//
// Before this, PlanRecord.FollowedProcedures was DECLARED in domain and READ here in
// the memory agent — and written by nobody. So `len(rec.FollowedProcedures) > 0` was
// never true, FeedProcedureOutcome never ran, and no routine's confidence ever moved.
// The tier could influence a plan and never learn whether it helped, which is the
// difference between a memory system and a cache with extra steps.
//
// The chain under test: enrichment.Procedures -> plan.FollowedProcedures (planner) ->
// PlanRecord.FollowedProcedures (executor) -> FeedProcedureOutcome (memory agent).
func TestProcedureFeedbackLoop_ClosesEndToEnd(t *testing.T) {
	t.Parallel()
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	// A routine that has worked so far, with enough evidence to be demotable.
	routine := domain.Procedure{
		ID:          "proc-under-test",
		Trigger:     "goal: ship the billing service | engages: 1 file",
		Steps:       []domain.ProcedureStep{{RequiredCapabilities: []string{"build"}, Intent: "perform: build"}},
		Confidence:  0.6,
		SampleCount: 3,
		Status:      domain.ProcedureActive,
	}
	if err := SaveProcedure(t.Context(), store, &recordingEmbedder{}, routine); err != nil {
		t.Fatalf("seed routine: %v", err)
	}

	// A plan that the routine informed, which then FAILED.
	p := mockPlan{
		id: "pf1", goal: "ship the billing service", touches: []string{"/srv/billing/main.go"}, success: false, surprise: 0.2,
	}
	p.followedProcedures = []string{routine.ID}
	runPlan(t, agent, p)

	doc, err := store.GetByID(t.Context(), routine.ID)
	if err != nil || doc == nil {
		t.Fatalf("routine vanished: %v", err)
	}
	got, ok := procedureFromDoc(*doc)
	if !ok {
		t.Fatalf("routine no longer parses: %+v", doc)
	}

	// A failure must LOWER confidence — the whole point of the loop.
	if !(got.Confidence < routine.Confidence) {
		t.Errorf("a failed plan must lower the routine's confidence: %.3f -> %.3f",
			routine.Confidence, got.Confidence)
	}
	if got.SampleCount != routine.SampleCount+1 {
		t.Errorf("the occurrence must be counted: %d -> %d", routine.SampleCount, got.SampleCount)
	}
	t.Logf("confidence %.3f -> %.3f, samples %d -> %d",
		routine.Confidence, got.Confidence, routine.SampleCount, got.SampleCount)
}
