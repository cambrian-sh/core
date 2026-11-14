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
	a := NewAgent(NewMemoryManager(docs, &recordingEmbedder{}), 3)
	a.RecordOutcomes = true
	a.ExperienceStore = exps
	return docs, exps, a
}

// engagePlan runs one mutation through the real tool-output path, which since
// ENG-1595 writes nothing (it used to write the mid-plan action record).
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ctx := context.Background()
	docs, exps, a := episodeAgent()

	_ = a.BeginExperience(ctx, "p-life")
	engagePlan(a, "p-life")
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{PlanID: "p-life", Goal: "do a thing", Success: true, Surprise: -1})

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
	// No plan-level chunk (scenes cancelled 2026-11-05) and no action record
	// (ENG-1595, 2026-11-14): the episode row is all a plan leaves.
	if len(docs.saved) != 0 {
		t.Errorf("a plan writes no chunk, got %d docs: %+v", len(docs.saved), docs.saved)
	}
}

// A plan that touched nothing (a pure-reasoning plan) still HAPPENED, and its row is
// already open. Returning early without closing it would leave the episode stuck at
// "running" forever.
func TestRecordPlanOutcome_PlanWithNoActionsStillClosesItsEpisode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs, exps, a := episodeAgent()

	_ = a.BeginExperience(ctx, "p-void")
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{PlanID: "p-void", Goal: "think about it", Success: false, Surprise: -1})

	if n := len(docs.saved); n != 0 {
		t.Errorf("an unsurprising failure with no actions writes no chunk, got %d", n)
	}
	if len(exps.saved) != 2 {
		t.Fatalf("expected the episode to be opened and closed, got %d writes", len(exps.saved))
	}
	if got := exps.saved[1].Outcome; got != "failure" {
		t.Errorf("the episode must reach a terminal outcome, got %q", got)
	}
}

// ENG-1595 (owner decision 2026-11-14): an episode still opens and closes, but no
// chunk hangs from it any more — neither the mid-plan action record nor the failure
// record is written.
func TestEpisode_NoRawRecordsHangFromIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs, exps, a := episodeAgent()
	_ = a.BeginExperience(ctx, "p-fail")
	engagePlan(a, "p-fail")
	_ = a.RecordPlanOutcome(ctx, domain.PlanRecord{
		PlanID: "p-fail", Goal: "migrate", Success: false, Surprise: 0.9, ReplanExhausted: true,
		FailureSummary: `step 2 (apply the migration): relation "documents" does not exist`,
	})
	if len(docs.saved) != 0 {
		t.Fatalf("no chunk may be written for the episode, got %+v", docs.saved)
	}
	if len(exps.saved) != 2 {
		t.Fatalf("the episode must still open and close, got %d experience writes", len(exps.saved))
	}
}
