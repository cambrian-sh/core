package executer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// captureSceneWriter records WriteScene calls for assertion.
type captureSceneWriter struct {
	calls     []domain.StepResult
	returnID  string
	returnErr error
	edgeCalls []specifyEdgeCall
}

type specifyEdgeCall struct {
	sourceID string
	targetID string
}

func (c *captureSceneWriter) WriteScene(_ context.Context, result domain.StepResult) (string, error) {
	c.calls = append(c.calls, result)
	return c.returnID, c.returnErr
}

func twoStepPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0"},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
}

func okStepFn(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result")}}, nil
}

// Cycle 1: nil SceneWriter → step completes without panic (tracer bullet).
func TestSceneWriter_NilIsNoop(t *testing.T) {
	ex := &DAGExecutor{SceneWriter: nil}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "step 0"}}}
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute with nil SceneWriter: %v", err)
	}
}

// captureRecorder records WritePlanScene calls (ADR-0049 D5).
//
// It keeps the WHOLE record, not just the goal: the PlanRecord is assembled
// field-by-field at the call site, so a field that stops being copied there is
// invisible to any assertion that only reads one other field.
type captureRecorder struct {
	planGoals []string
	recs      []domain.PlanRecord
	// calls is the ORDER of memory calls, so "the episode was opened first" is
	// assertable — the whole point of BeginExperience is that it precedes the records
	// that reference it (ADR-0095 D1).
	calls  []string
	begins []string // planIDs passed to BeginExperience
}

func (c *captureRecorder) BeginExperience(_ context.Context, planID string) error {
	c.calls = append(c.calls, "begin")
	c.begins = append(c.begins, planID)
	return nil
}

func (c *captureRecorder) RecordExecution(_ context.Context, _ domain.StepResult) error {
	c.calls = append(c.calls, "record")
	return nil
}
func (c *captureRecorder) WritePlanScene(_ context.Context, rec domain.PlanRecord) error {
	c.calls = append(c.calls, "scene")
	c.planGoals = append(c.planGoals, rec.Goal)
	c.recs = append(c.recs, rec)
	return nil
}

// ADR-0049 D5: scenes are no longer per-step — the per-step WriteScene is not called.
func TestScenes_NoLongerWrittenPerStep(t *testing.T) {
	sw := &captureSceneWriter{returnID: "scene-id-1"}
	ex := &DAGExecutor{SceneWriter: sw}

	if _, err := ex.Execute(context.Background(), twoStepPlan(), nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(sw.calls) != 0 {
		t.Errorf("per-step WriteScene must not be called (scenes are plan-wide); got %d", len(sw.calls))
	}
}

// ADR-0049 D5: exactly one plan scene is written at completion, carrying the goal.
func TestWritePlanScene_OncePerPlan(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := twoStepPlan()
	plan.Subject = "the plan goal"
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.planGoals) != 1 || rec.planGoals[0] != "the plan goal" {
		t.Errorf("expected exactly one plan scene with the goal; got %v", rec.planGoals)
	}
}

// ADR-0095 D1: the episode is opened at plan START, once, before any record that
// references it.
//
// The ORDER is the whole assertion. The parent id is derived from the plan id "so
// records written mid-plan can reference a parent that already exists" — and the FK is
// nullable, resolved through a subselect, so minting it late fails silently: every
// mid-plan action record lands with a NULL parent and the episode can never be listed,
// deleted or governed as one thing. Nothing observable breaks; the row is just wrong.
func TestBeginExperience_OpensTheEpisodeAtPlanStart(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	if _, err := ex.Execute(context.Background(), twoStepPlan(), nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.begins) != 1 {
		t.Fatalf("expected exactly one episode per plan execution, got %d: %v", len(rec.begins), rec.begins)
	}
	if rec.begins[0] == "" {
		t.Error("the episode must be opened with the plan id its records derive their parent from")
	}
	if len(rec.calls) == 0 || rec.calls[0] != "begin" {
		t.Errorf("the episode must be opened before any record referencing it; call order was %v", rec.calls)
	}
	// And it is still closed exactly once, by the completion write.
	if len(rec.recs) != 1 {
		t.Errorf("expected one plan scene closing the episode, got %d", len(rec.recs))
	}
}

// ADR-0094 D8: the routines that informed the plan must reach the PlanRecord, or the
// memory agent's co-evolution branch (`len(rec.FollowedProcedures) > 0`) is
// unreachable and no routine's confidence ever moves from an outcome.
//
// This asserts the EXECUTOR hop specifically. The end-to-end feedback test in
// internal/memory builds its PlanRecord directly, so it passes with this hop deleted —
// which it was, for as long as the field existed.
func TestWritePlanScene_CarriesFollowedProcedures(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := twoStepPlan()
	plan.FollowedProcedures = []string{"routine-a", "routine-b"}
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	got := rec.recs[0].FollowedProcedures
	if len(got) != 2 || got[0] != "routine-a" || got[1] != "routine-b" {
		t.Errorf("PlanRecord dropped the followed routines: got %v, want [routine-a routine-b]", got)
	}
}

// ADR-0049 A2.2: the failed step and its error exist only in the executor, so this hop is
// the only place they can reach the record. Without it the failure precedent stores the
// situation and nothing about the failure.
func TestWritePlanScene_CarriesTheFailureMode(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "read the config"},
		{Query: "apply the migration", DependsOn: []int{0}},
	}}
	stepFn := dispatchingStep(map[int]StepFunc{
		0: okStep("result_0", nil),
		1: failStep("relation \"documents\" does not exist"),
	})
	if _, err := ex.Execute(t.Context(), plan, nil, stepFn); err == nil {
		t.Fatal("expected the plan to fail")
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	got := rec.recs[0]
	if got.Success {
		t.Error("a failed plan must not be recorded as a success")
	}
	if got.FailedStep != 1 {
		t.Errorf("FailedStep: want 1, got %d", got.FailedStep)
	}
	if !containsStr(got.FailureSummary, "step 1") ||
		!containsStr(got.FailureSummary, "apply the migration") ||
		!containsStr(got.FailureSummary, "does not exist") {
		t.Errorf("FailureSummary must name the step, its query and the error; got %q", got.FailureSummary)
	}
}

// A successful plan carries no failure mode — an empty summary is what lets the memory
// agent tell "nothing went wrong" from "something did, unrendered".
func TestWritePlanScene_SuccessCarriesNoFailureMode(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	if _, err := ex.Execute(t.Context(), twoStepPlan(), nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	if rec.recs[0].FailureSummary != "" {
		t.Errorf("a successful plan must carry no failure summary; got %q", rec.recs[0].FailureSummary)
	}
}

// Replan exhaustion is decided by the executor's counters and exists nowhere else, so
// this hop is the only place it can reach the record. The memory agent uses it to write
// a failure precedent that the A2.3 surprise gate would otherwise swallow — a plan that
// ran the whole recovery ladder and still failed is worth remembering whether or not
// merit predicted it, which is the cold-start case.
func TestWritePlanScene_CarriesReplanExhaustion(t *testing.T) {
	rec := &captureRecorder{}
	// MaxReplanAttempts 0 with no ReplanHandler: the ladder has no rungs left, so the
	// single replan the plan consumed already exhausts it.
	ex := &DAGExecutor{MemoryRecorder: rec, MaxReplanAttempts: 0}

	// The replan is consumed from INSIDE the step, so it is ordered before the
	// coordinator observes the step's result — no sleeping, no shared-state race.
	stepFn := StepFunc(func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		ex.HotSwap(&domain.ExecutionPlan{Steps: []domain.Step{{Query: "recover"}}})
		return nil, errors.New("relation \"documents\" does not exist")
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "apply the migration"}}}
	if _, err := ex.Execute(t.Context(), plan, nil, stepFn); err == nil {
		t.Fatal("expected the plan to fail")
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	if rec.recs[0].Success {
		t.Error("a failed plan must not be recorded as a success")
	}
	if !rec.recs[0].ReplanExhausted {
		t.Error("a plan that outran its replan budget must carry ReplanExhausted; " +
			"without it the cold-start failure precedent is never written")
	}
}

// The counterpart, and what keeps the carve-out narrow: an ordinary single failure that
// never replanned must NOT claim exhaustion, or the memory agent's carve-out degenerates
// into "write a precedent for every failure" and the surprise gate stops meaning anything.
func TestWritePlanScene_PlainFailureIsNotReplanExhausted(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "read the config"},
		{Query: "apply the migration", DependsOn: []int{0}},
	}}
	stepFn := dispatchingStep(map[int]StepFunc{
		0: okStep("result_0", nil),
		1: failStep("connection refused"),
	})
	if _, err := ex.Execute(t.Context(), plan, nil, stepFn); err == nil {
		t.Fatal("expected the plan to fail")
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	if rec.recs[0].ReplanExhausted {
		t.Error("a plan that never replanned must not be recorded as replan-exhausted")
	}
}

// And a success carries false, so the memory agent's !success clause is never the only
// thing standing between a clean plan and a negative edge.
func TestWritePlanScene_SuccessIsNotReplanExhausted(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	if _, err := ex.Execute(t.Context(), twoStepPlan(), nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	if rec.recs[0].ReplanExhausted {
		t.Error("a successful plan must not be recorded as replan-exhausted")
	}
}

// capturePlanEvents keeps every ADR-0021 plan-level telemetry row.
type capturePlanEvents struct{ events []domain.PlanEvent }

func (c *capturePlanEvents) WritePlanEvent(e domain.PlanEvent) error {
	c.events = append(c.events, e)
	return nil
}

// TestReplanExhaustion_RunsTheCompletionBlock is the end-to-end shape of the real
// exhaustion path — replanning through the ReplanHandler until MaxReplanAttempts is
// spent — and it is a REGRESSION test for three defects that shared one cause.
//
// The exhaustion exits used to `return` from inside the coordinator loop, above the
// completion block. So a plan that ran its whole recovery ladder and failed produced:
// no ADR-0021 PlanEvent, no plan scene, and — since ADR-0095 D1 opens the episode at
// plan START — an experiences row left at outcome "running" forever. Precisely the
// failure most worth recording was the one that recorded nothing.
//
// The caller's error is asserted too: the fix moves WHERE these paths exit from, never
// what they return.
func TestReplanExhaustion_RunsTheCompletionBlock(t *testing.T) {
	rec := &captureRecorder{}
	events := &capturePlanEvents{}
	ex := &DAGExecutor{
		MemoryRecorder:    rec,
		PlanEventWriter:   events,
		ReplanHandler:     &fixedReplanHandler{query: "a genuinely different recovery step"},
		MaxReplanAttempts: 1,
	}

	// Every step fails, so: fail → replan #1 (within budget, applied) → fail again →
	// replan #2 exceeds MaxReplanAttempts → the ladder is spent.
	plan := &domain.ExecutionPlan{Subject: "migrate the store", Steps: []domain.Step{{Query: "the step that fails"}}}
	_, err := ex.Execute(t.Context(), plan, nil, failStep("relation \"documents\" does not exist"))

	// (c) the caller still gets the same PartialPlanError it always did.
	var partial *PartialPlanError
	if !errors.As(err, &partial) {
		t.Fatalf("expected a *PartialPlanError, got %#v", err)
	}
	if partial.ReplanCount != 2 {
		t.Errorf("ReplanCount: want 2, got %d", partial.ReplanCount)
	}
	if partial.LastError == nil || !containsStr(partial.LastError.Error(), "does not exist") {
		t.Errorf("the step error must survive as LastError; got %v", partial.LastError)
	}

	// (a) exactly one plan scene, marked failed and replan-exhausted.
	if len(rec.recs) != 1 {
		t.Fatalf("expected exactly one plan scene; got %d", len(rec.recs))
	}
	got := rec.recs[0]
	if got.Success {
		t.Error("an exhausted plan must not be recorded as a success")
	}
	if !got.ReplanExhausted {
		t.Error("the exhausted recovery ladder must reach the record, or the failure " +
			"precedent stays behind the surprise gate it was carved out of")
	}
	// The episode was opened at plan start and CLOSED by this write — the stuck-at-
	// "running" row is the defect that made this more than a missing precedent.
	if len(rec.begins) != 1 {
		t.Fatalf("expected one opened episode; got %d", len(rec.begins))
	}
	if len(rec.calls) < 2 || rec.calls[0] != "begin" || rec.calls[len(rec.calls)-1] != "scene" {
		t.Errorf("the episode must be opened first and closed by the completion write; call order was %v", rec.calls)
	}

	// (b) exactly one PlanEvent, stamped replan_exhausted.
	if len(events.events) != 1 {
		t.Fatalf("expected exactly one PlanEvent; got %d", len(events.events))
	}
	if events.events[0].Outcome != domain.PlanOutcomeReplanExhausted {
		t.Errorf("PlanEvent outcome: want %q, got %q",
			domain.PlanOutcomeReplanExhausted, events.events[0].Outcome)
	}
	if events.events[0].ReplanCount != 2 {
		t.Errorf("PlanEvent ReplanCount: want 2, got %d", events.events[0].ReplanCount)
	}
}

// The loop-guard rejection is the other exit that used to skip the completion block. Its
// error is REPLACED at the exit (the rejection becomes the plan's error), so this pins
// both halves: the caller still sees the guard's message, and the plan still completes
// its bookkeeping.
func TestReplanLoopGuardRejection_StillCompletesTheEpisode(t *testing.T) {
	rec := &captureRecorder{}
	events := &capturePlanEvents{}
	ex := &DAGExecutor{
		MemoryRecorder:    rec,
		PlanEventWriter:   events,
		ReplanHandler:     &fixedReplanHandler{query: "the step that fails"}, // restates the failure
		MaxReplanAttempts: 2,
	}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "the step that fails"}}}
	_, err := ex.Execute(t.Context(), plan, nil, failStep("boom"))
	if err == nil || !containsStr(err.Error(), "repeats the same step") {
		t.Fatalf("expected the loop-guard error, got: %v", err)
	}
	if len(rec.recs) != 1 || rec.recs[0].Success {
		t.Fatalf("expected exactly one plan scene, marked failed; got %+v", rec.recs)
	}
	if len(events.events) != 1 {
		t.Fatalf("expected exactly one PlanEvent; got %d", len(events.events))
	}
}

func TestSummarizeFailure(t *testing.T) {
	steps := []domain.Step{{Query: "read the config"}, {Query: "apply the migration"}}

	t.Run("nil error renders nothing", func(t *testing.T) {
		if got := summarizeFailure(steps, 0, nil); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})

	t.Run("names the step, its query and the error", func(t *testing.T) {
		got := summarizeFailure(steps, 1, errors.New("connection refused"))
		if want := "step 1 (apply the migration): connection refused"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("an unreachable step index drops only the query", func(t *testing.T) {
		// A replan reassigns plan.Steps, so the index can outlive the slice it names.
		got := summarizeFailure(steps, 7, errors.New("boom"))
		if want := "step 7: boom"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if got := summarizeFailure(nil, -1, errors.New("boom")); got != "step -1: boom" {
			t.Errorf("negative index: got %q", got)
		}
	})

	t.Run("an empty query drops only the query", func(t *testing.T) {
		got := summarizeFailure([]domain.Step{{Query: "   "}}, 0, errors.New("boom"))
		if want := "step 0: boom"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("bounded on a rune boundary", func(t *testing.T) {
		// Multi-byte throughout: a byte-wise cut would leave a replacement rune, which
		// makes the same failure render two different ways depending on where it landed.
		long := strings.Repeat("é", 500)
		got := summarizeFailure([]domain.Step{{Query: long}}, 0, errors.New(long))
		if n := len([]rune(got)); n != maxFailureSummaryRunes {
			t.Errorf("want %d runes, got %d", maxFailureSummaryRunes, n)
		}
		if !utf8.ValidString(got) {
			t.Error("bound must cut on a rune boundary")
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Error("bound split a multi-byte rune")
		}
	})

	t.Run("a long query cannot crowd out the error", func(t *testing.T) {
		got := summarizeFailure([]domain.Step{{Query: strings.Repeat("q", 400)}}, 0, errors.New("the real error"))
		if !containsStr(got, "the real error") {
			t.Errorf("the error text must survive a long query; got %q", got)
		}
	})
}
