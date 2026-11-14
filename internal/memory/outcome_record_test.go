package memory

import (
	"context"
	"strings"
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

// TestRecordPlanOutcome_SurpriseGatesTheFailurePrecedent covers ADR-0049 A2.3's core
// distinction. An UNSURPRISING failure closes its episode and writes nothing more; a
// surprising one earns a failure record in the negative-edge lane. A gate that fired on every failure would be a failure gate, which is exactly
// what A2.3 rejects.
func TestRecordPlanOutcome_SurpriseGatesTheFailurePrecedent(t *testing.T) {
	t.Parallel()
	newAgent := func() (*multiCaptureStore, *Agent) {
		store := &multiCaptureStore{}
		a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.7, 5, 3)
		a.RecordOutcomes = true
		a.SurpriseFloor = 0.5
		return store, a
	}
	negEdges := func(s *multiCaptureStore) int {
		n := 0
		for _, d := range s.saved {
			if d.DocumentType == domain.DocTypeNegativeEdge {
				n++
			}
		}
		return n
	}
	negEdge := func(t *testing.T, s *multiCaptureStore) domain.Document {
		t.Helper()
		for _, d := range s.saved {
			if d.DocumentType == domain.DocTypeNegativeEdge {
				return d
			}
		}
		t.Fatal("no failure precedent written")
		return domain.Document{}
	}
	engage := func(a *Agent, plan string) {
		_ = a.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
			ToolName: "write_file", ArgsJSON: []byte(`{"path":"/tmp/x.md"}`),
			Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-" + plan,
		})
	}

	t.Run("unsurprising failure writes no precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p1")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "p1", Goal: "routine", Success: false, Surprise: 0.1})
		if n := negEdges(store); n != 0 {
			t.Errorf("a predicted failure must not earn a precedent, got %d", n)
		}
	})

	t.Run("surprising failure writes a precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p2")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "p2", Goal: "shock", Success: false, Surprise: 0.9})
		if n := negEdges(store); n != 1 {
			t.Errorf("an unexpected failure must earn exactly one precedent, got %d", n)
		}
	})

	// ADR-0049 A2.2 shapes the precedent as conditions → attempt → failure mode. The
	// goal is the conditions; without the executor's failure summary the stored record
	// names only the goal, so every failure of one goal is the same precedent and
	// retrieval can never say HOW it went wrong.
	t.Run("the precedent carries the failure mode", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p4")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p4", Goal: "migrate", Success: false, Surprise: 0.9,
			FailedStep:     2,
			FailureSummary: `step 2 (apply the migration): relation "documents" does not exist`,
		})
		edge := negEdge(t, store)
		for _, want := range []string{"step 2", "apply the migration", "does not exist"} {
			if !strings.Contains(edge.Text, want) {
				t.Errorf("precedent text missing %q; got %q", want, edge.Text)
			}
		}
		// The conditions half survives: since 2026-11-05 it is built from the
		// PlanRecord (goal and outcome), not from the removed scene text.
		if !strings.Contains(edge.Text, "plan goal: migrate | outcome: failure") {
			t.Errorf("precedent dropped the conditions; got %q", edge.Text)
		}
	})

	t.Run("an unrendered failure falls back to the goal", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p5")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p5", Goal: "migrate", Success: false, Surprise: 0.9,
		})
		edge := negEdge(t, store)
		if !strings.Contains(edge.Text, "unexpected plan failure: migrate") {
			t.Errorf("empty summary must fall back to the goal; got %q", edge.Text)
		}
	})

	// The COLD-START case, and the reason the carve-out exists. The surprise oracle
	// returns "unknown" (-1) for an agent with no merit history, so a new agent can never
	// clear the floor — without this branch its earliest failures, the most instructive
	// ones there are, leave no precedent at all. Replan exhaustion means the full
	// recovery ladder was run and still failed, which stands on its own as evidence.
	t.Run("replan exhaustion writes a precedent with surprise unknown", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p6")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p6", Goal: "cold start", Success: false, Surprise: -1,
			ReplanExhausted: true,
			FailureSummary:  `step 0 (write the file): permission denied`,
		})
		if n := negEdges(store); n != 1 {
			t.Fatalf("an exhausted recovery ladder must earn exactly one precedent, got %d", n)
		}
		// Through the same parented helper as the surprise path: the carve-out must not
		// be a second, unparented write.
		edge := negEdge(t, store)
		if !strings.Contains(edge.Text, "permission denied") {
			t.Errorf("precedent dropped the failure mode; got %q", edge.Text)
		}
		if pid, _ := edge.Metadata["plan_id"].(string); pid != "p6" {
			t.Errorf("precedent must be joinable back to its plan; plan_id=%v", edge.Metadata["plan_id"])
		}
	})

	// And the carve-out must not depend on the floor being configured at all — an
	// unconfigured deployment is exactly the cold-start case being fixed.
	t.Run("replan exhaustion does not need a configured floor", func(t *testing.T) {
		store, a := newAgent()
		a.SurpriseFloor = 0 // never set by this deployment
		engage(a, "p7")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p7", Goal: "cold start", Success: false, Surprise: -1, ReplanExhausted: true,
		})
		if n := negEdges(store); n != 1 {
			t.Errorf("the carve-out must be independent of SurpriseFloor, got %d precedents", n)
		}
	})

	// The gate itself stays intact: without exhaustion, an under-floor failure is still
	// silent. This is what keeps the change a carve-out rather than a failure gate.
	t.Run("an under-floor failure without exhaustion still writes nothing", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p8")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p8", Goal: "routine", Success: false, Surprise: 0.1, ReplanExhausted: false,
		})
		if n := negEdges(store); n != 0 {
			t.Errorf("a predicted failure that never exhausted replans must stay silent, got %d", n)
		}
	})

	// Impossible in the executor (exhaustion implies a failure) and guarded anyway: the
	// !success clause, not the carve-out, is what makes a negative edge negative.
	t.Run("a SUCCESS marked replan-exhausted writes no precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p9")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{
			PlanID: "p9", Goal: "recovered", Success: true, Surprise: -1, ReplanExhausted: true,
		})
		if n := negEdges(store); n != 0 {
			t.Errorf("a success is not a negative edge however it was reached, got %d", n)
		}
	})

	t.Run("surprising SUCCESS writes no precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p3")
		_ = a.RecordPlanOutcome(context.Background(), domain.PlanRecord{PlanID: "p3", Goal: "pleasant", Success: true, Surprise: 0.9})
		if n := negEdges(store); n != 0 {
			t.Errorf("a success is not a negative edge however surprising, got %d", n)
		}
	})
}

// decision (2026-11-05, scenes cancelled): the failure record and the procedure
// feedback used to sit BELOW the "contentless scene" early return, so a plan that
// engaged no entity (a pure-reasoning plan, or a tool call whose args did not parse)
// left no failure record and never fed its procedures. With the scene gone there is
// no early return: both run for every plan.
func TestRecordPlanOutcome_NoEngagementStillWritesFailureRecordAndFeedsProcedures(t *testing.T) {
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
	a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.7, 5, 3)
	a.RecordOutcomes = true
	a.ProcedureDeprecateBelow = 0.3

	// No RecordToolOutput call: the plan engaged nothing.
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{
		PlanID: "p-pure", Goal: "reason about it", Success: false, Surprise: -1,
		ReplanExhausted: true, FailureSummary: "step 0 (think): gave up",
		FollowedProcedures: []string{"proc-r"},
	})

	edges := store.ofType(domain.DocTypeNegativeEdge)
	if len(edges) != 1 {
		t.Fatalf("a replan-exhausted failure with no engaged entity must still write its failure record, got %d", len(edges))
	}
	for _, want := range []string{"gave up", "plan goal: reason about it", "replan exhausted"} {
		if !strings.Contains(edges[0].Text, want) {
			t.Errorf("failure record missing %q; got %q", want, edges[0].Text)
		}
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

// TestRecordPlanOutcome_RetriedFailureUpsertsOneRow is the AGENTS.md §2.6 regression
// (T-20261113-memory-agent-mints-clock-derived-ids-for): the failure precedent's id was
// "neg-edge-<UnixNano>", so a retried RecordPlanOutcome wrote a second row. The id is
// now derived from the plan, the agent and the record text, so a retry carries the same
// id (the store upserts it) while a different plan, or a different failure of the same
// plan, still gets its own row.
func TestRecordPlanOutcome_RetriedFailureUpsertsOneRow(t *testing.T) {
	t.Parallel()
	store := &multiCaptureStore{}
	a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.7, 5, 3)
	a.RecordOutcomes = true
	a.SurpriseFloor = 0.5
	rec := domain.PlanRecord{PlanID: "p-retry", Goal: "migrate", Success: false, Surprise: 0.9,
		FailureSummary: "step 1: boom"}
	for i := 0; i < 2; i++ {
		if err := a.RecordPlanOutcome(context.Background(), rec); err != nil {
			t.Fatalf("RecordPlanOutcome #%d: %v", i, err)
		}
	}
	other := rec
	other.PlanID = "p-other"
	_ = a.RecordPlanOutcome(context.Background(), other)
	differentFailure := rec
	differentFailure.FailureSummary = "step 3: a different failure"
	_ = a.RecordPlanOutcome(context.Background(), differentFailure)

	var ids []string
	for _, d := range store.saved {
		if d.DocumentType == domain.DocTypeNegativeEdge {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("want 4 precedent writes, got %d: %v", len(ids), ids)
	}
	if ids[0] != ids[1] {
		t.Errorf("a retried outcome must reuse its id (upsert), got %q then %q", ids[0], ids[1])
	}
	if ids[2] == ids[0] || ids[3] == ids[0] || ids[2] == ids[3] {
		t.Errorf("distinct plans / failures must keep distinct ids, got %v", ids)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "neg-edge-") {
			t.Errorf("id %q lost the neg-edge- prefix", id)
		}
	}
}

// The unparented port path (IngestNegativeEdge) is deterministic too.
func TestIngestNegativeEdge_IDIsContentDerived(t *testing.T) {
	t.Parallel()
	store := &multiCaptureStore{}
	a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.7, 5, 3)
	_ = a.IngestNegativeEdge(context.Background(), "tool crashed", "out", "agent-a")
	_ = a.IngestNegativeEdge(context.Background(), "tool crashed", "out", "agent-a")
	_ = a.IngestNegativeEdge(context.Background(), "tool crashed", "out", "agent-b")
	if len(store.saved) != 3 {
		t.Fatalf("want 3 writes, got %d", len(store.saved))
	}
	if store.saved[0].ID != store.saved[1].ID {
		t.Errorf("same report twice must reuse its id, got %q / %q", store.saved[0].ID, store.saved[1].ID)
	}
	if store.saved[0].ID == store.saved[2].ID {
		t.Errorf("a different reporting agent must get a different id")
	}
}
