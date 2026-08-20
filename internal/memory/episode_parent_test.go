package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0095 D1: the EPISODE PARENT.
//
// An `experiences` row is the parent of ONE plan execution's records, and
// `chunks.experience_id` cascades from it — so "forget an episode" is one delete. That
// only holds if every record of the episode actually points at it, and it did not: the
// parent was minted at plan COMPLETION while action records are written mid-plan, and
// the adapter resolves a missing parent to NULL rather than failing the insert. The gap
// was therefore invisible everywhere except a query that expected the episode to be
// whole — no error, no warning, just an episode in pieces.
//
// These tests use the fakes from outcome_record_test.go (same package): the experience
// store keeps EVERY call, unlike memory_lifecycle_test.go's map, which keeps only the
// last and so cannot show what the completion write did to what the start write recorded.

// episodeAgent is the smallest agent that writes a real episode: the outcome arm on, a
// capturing document store, and a call-keeping experience store.
func episodeAgent() (*multiCaptureStore, *fakeExperienceStore, *Agent) {
	docs := &multiCaptureStore{}
	exps := &fakeExperienceStore{}
	a := NewAgent(NewMemoryManager(docs, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
	a.RecordOutcomes = true
	a.SurpriseFloor = 0.5
	a.ExperienceStore = exps
	return docs, exps, a
}

// engagePlan runs one mutation through the real tool-output path — which is what both
// accretes the plan's engaged scope and writes the mid-plan action record.
func engagePlan(a *Agent, planID string) {
	_ = a.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"/tmp/ep.md"}`),
		Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-" + planID,
	})
}

func docsOfType(s *multiCaptureStore, docType string) []domain.Document {
	var out []domain.Document
	for _, d := range s.saved {
		if d.DocumentType == docType {
			out = append(out, d)
		}
	}
	return out
}

// The episode must exist BEFORE the plan's records, carrying its start and its born
// tags, and marked still running — a terminal outcome here would be a claim about a
// plan that has not executed a step yet.
func TestBeginExperience_OpensTheEpisodeBeforeItsRecords(t *testing.T) {
	_, exps, a := episodeAgent()
	if err := a.BeginExperience(context.Background(), "p-begin"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exps.saved) != 1 {
		t.Fatalf("expected exactly one episode minted at plan start, got %d", len(exps.saved))
	}
	got := exps.saved[0]
	if got.ID != "exp-p-begin" {
		t.Errorf("the id must be derived from the plan id, got %q", got.ID)
	}
	if got.StartedAt.IsZero() {
		t.Error("an opened episode must record when it started")
	}
	if !got.CompletedAt.IsZero() {
		t.Errorf("an unfinished episode must carry no completion time, got %v", got.CompletedAt)
	}
	if got.Outcome != outcomeRunning {
		t.Errorf("expected outcome %q while in flight, got %q", outcomeRunning, got.Outcome)
	}
	// D4: born tagged, or the boundary is inexpressible rather than merely open.
	if len(got.Tags) == 0 {
		t.Error("an episode cannot be born untagged")
	}
}

// The episode parent is part of the default-off arm, not an always-on side table.
func TestBeginExperience_WritesNothingWhenThereIsNothingToParent(t *testing.T) {
	t.Run("arm off", func(t *testing.T) {
		store := &fakeExperienceStore{}
		a := &Agent{ExperienceStore: store} // RecordOutcomes defaults false
		if err := a.BeginExperience(context.Background(), "p1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(store.saved) != 0 {
			t.Errorf("arm off must mint nothing, got %+v", store.saved)
		}
	})
	t.Run("empty plan id", func(t *testing.T) {
		store := &fakeExperienceStore{}
		a := &Agent{RecordOutcomes: true, ExperienceStore: store}
		if err := a.BeginExperience(context.Background(), ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(store.saved) != 0 {
			t.Errorf("an empty plan id is not an episode, got %+v", store.saved)
		}
	})
	t.Run("nil store", func(t *testing.T) {
		a := &Agent{RecordOutcomes: true} // no ExperienceStore wired
		if err := a.BeginExperience(context.Background(), "p1"); err != nil {
			t.Fatalf("a deployment without an experience store must be a no-op, got %v", err)
		}
	})
}

// Completion CLOSES the episode the start opened: same id, terminal outcome, completion
// time — and it must not erase when the work began. The Postgres upsert leaves
// started_at alone on conflict, but the Go-side struct is what a store that overwrote
// every column would receive, and it is the only place this is checkable without a
// database.
func TestExperience_CompletionClosesWithoutErasingItsStart(t *testing.T) {
	ctx := context.Background()
	docs, exps, a := episodeAgent()

	_ = a.BeginExperience(ctx, "p-life")
	engagePlan(a, "p-life")
	_ = a.WritePlanScene(ctx, domain.PlanRecord{PlanID: "p-life", Goal: "do a thing", Success: true, Surprise: -1})

	if len(exps.saved) != 2 {
		t.Fatalf("expected an open and a close for one episode, got %d writes: %+v", len(exps.saved), exps.saved)
	}
	begun, closed := exps.saved[0], exps.saved[1]
	if closed.ID != begun.ID {
		t.Fatalf("both writes must address ONE episode: %q then %q", begun.ID, closed.ID)
	}
	if closed.Outcome != "success" {
		t.Errorf("completion must record the terminal outcome, got %q", closed.Outcome)
	}
	if closed.CompletedAt.IsZero() {
		t.Error("completion must record when the episode ended")
	}
	if !closed.StartedAt.Equal(begun.StartedAt) {
		t.Errorf("closing an episode must carry its start forward: %v → %v", begun.StartedAt, closed.StartedAt)
	}
	scenes := docsOfType(docs, domain.DocTypeMnemonicScene)
	if len(scenes) != 1 || scenes[0].ExperienceID != "exp-p-life" {
		t.Errorf("the outcome record must hang from the episode, got %+v", scenes)
	}
}

// A plan that engaged nothing writes no SCENE (ADR-0049 D5/D7) — but it still HAPPENED,
// and its row is already open. Returning early without closing it would leave the
// episode stuck at "running" forever, with whatever it did write hanging off a parent
// that never reaches a terminal state.
func TestWritePlanScene_ContentlessPlanStillClosesItsEpisode(t *testing.T) {
	ctx := context.Background()
	docs, exps, a := episodeAgent()

	_ = a.BeginExperience(ctx, "p-void")
	_ = a.WritePlanScene(ctx, domain.PlanRecord{PlanID: "p-void", Goal: "think about it", Success: false, Surprise: -1})

	if n := len(docsOfType(docs, domain.DocTypeMnemonicScene)); n != 0 {
		t.Errorf("a plan with no engaged entities must write no scene, got %d", n)
	}
	if len(exps.saved) != 2 {
		t.Fatalf("expected the episode to be opened and closed, got %d writes", len(exps.saved))
	}
	if got := exps.saved[1].Outcome; got != "failure" {
		t.Errorf("the episode must reach a terminal outcome, got %q", got)
	}
}

// The action record is written MID-plan, and is what exposed the defect: it carried
// plan_id in metadata and no parent at all, so the cascade that defines the episode
// skipped the whole action path.
func TestActionRecord_HangsFromTheEpisode(t *testing.T) {
	t.Run("arm on: parented", func(t *testing.T) {
		docs, _, a := episodeAgent()
		_ = a.BeginExperience(context.Background(), "p-act")
		engagePlan(a, "p-act")

		actions := docsOfType(docs, domain.DocTypeMnemonicAction)
		if len(actions) != 1 {
			t.Fatalf("expected one action record, got %d", len(actions))
		}
		if actions[0].ExperienceID != "exp-p-act" {
			t.Errorf("a mid-plan action must hang from its episode, got %q", actions[0].ExperienceID)
		}
		if got, _ := actions[0].Metadata["plan_id"].(string); got != "p-act" {
			t.Errorf("the join key must survive alongside the parent, got %q", got)
		}
	})

	// With the arm off nothing is written at all — not "written unparented".
	t.Run("arm off: nothing written", func(t *testing.T) {
		docs := &multiCaptureStore{}
		a := NewAgent(NewMemoryManager(docs, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
		engagePlan(a, "p-off")
		if len(docs.saved) != 0 {
			t.Errorf("both arms off must write nothing, got %+v", docs.saved)
		}
	})
}

// The failure precedent belongs to its episode and must be deletable with it. The other
// negative-edge callers do NOT: a Tier-2 commit error or a reported agent fault is not
// one plan execution, and inventing a parent for it would be a claim about something
// that never happened.
func TestFailurePrecedent_HangsFromTheEpisodeAndOtherEdgesDoNot(t *testing.T) {
	ctx := context.Background()
	docs, _, a := episodeAgent()

	_ = a.BeginExperience(ctx, "p-fail")
	engagePlan(a, "p-fail")
	_ = a.WritePlanScene(ctx, domain.PlanRecord{
		PlanID: "p-fail", Goal: "migrate", Success: false, Surprise: 0.9,
		FailureSummary: `step 2 (apply the migration): relation "documents" does not exist`,
	})

	edges := docsOfType(docs, domain.DocTypeNegativeEdge)
	if len(edges) != 1 {
		t.Fatalf("expected exactly one failure precedent, got %d", len(edges))
	}
	if edges[0].ExperienceID != "exp-p-fail" {
		t.Errorf("the failure precedent must hang from its episode, got %q", edges[0].ExperienceID)
	}
	if got, _ := edges[0].Metadata["plan_id"].(string); got != "p-fail" {
		t.Errorf("the precedent must be joinable to its plan, got %q", got)
	}

	// The port-level call is unchanged: no parent, no plan_id.
	bare := &multiCaptureStore{}
	other := NewAgent(NewMemoryManager(bare, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
	if err := other.IngestNegativeEdge(ctx, "tier-2 commit failed", "last output", "agent-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	loose := docsOfType(bare, domain.DocTypeNegativeEdge)
	if len(loose) != 1 {
		t.Fatalf("expected one negative edge, got %d", len(loose))
	}
	if loose[0].ExperienceID != "" {
		t.Errorf("a failure that is not one plan execution must stay unparented, got %q", loose[0].ExperienceID)
	}
	if _, ok := loose[0].Metadata["plan_id"]; ok {
		t.Errorf("no plan_id may be invented for it, got %v", loose[0].Metadata["plan_id"])
	}
}
