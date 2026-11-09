package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// The staged planner (ADR-0145 D11, D5a; T-20261108-adr-0145-d11-staged-planner-returns-next-stage).
//
// Under execution.plan.staged_planner the planner is called once per stage with a
// short, fresh context (the checklist with item statuses, a compact index of the
// run's ledger, the earlier stages' step results as references, today's calendar)
// and answers with exactly one of:
//
//   - the next stage: a small DAG the executor runs (StageOutcomeStage);
//   - a question for the person (StageOutcomeInputRequired): facts only the person
//     holds or decisions only the person can make, never something a tool or the
//     ledger could look up;
//   - nothing more to add (StageOutcomeNothingMore).
//
// It never declares the work done: completion and stalls are decided by code from
// item statuses (requirements.Coverage, D5). Between its own calls the planner may
// look into the ledger (StageOutcomeRead): the kernel serves the reads with the
// ledger_get / ledger_find kernel tools under the caller's read predicate and
// calls the planner again with the results. The planner has no tool loop of its
// own, so this works on providers that pass no native tool choice (the claudebox
// proxy, ADR-0145 observations).

// Stage outcomes in the planner's reply (planner_cap v8 "outcome").
const (
	StageOutcomeStage         = "stage"
	StageOutcomeInputRequired = "input_required"
	StageOutcomeNothingMore   = "nothing_more"
	// StageOutcomeRead asks the kernel for ledger reads before deciding; it never
	// leaves the planner (GetExecutionPlan serves it and calls the model again).
	StageOutcomeRead = "read"
)

// Question kinds (D5a.1: only what the person holds or must decide).
const (
	QuestionMissingParameter = "missing_parameter"
	QuestionAmbiguity        = "ambiguity"
	QuestionConflict         = "conflict"
	QuestionDecision         = "decision"
)

// ValidQuestionKind reports whether k is one of the four question kinds.
func ValidQuestionKind(k string) bool {
	switch k {
	case QuestionMissingParameter, QuestionAmbiguity, QuestionConflict, QuestionDecision:
		return true
	}
	return false
}

// InputQuestion is one question for the person. Text is English (internal
// language); the chat agent asks it in the person's language
// (decisions/internal-english-local-language-at-the-edges.md). Quote, when set,
// is the span of the person's own message the question is about, verbatim.
type InputQuestion struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Kind     string   `json:"kind"`
	Items    []string `json:"items,omitempty"`
	Quote    string   `json:"quote,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// InputRequiredVersion is the shape version of InputRequired.
const InputRequiredVersion = 1

// HandoffKeyInputRequired is the Execute response metadata key that carries an
// InputRequired as JSON when a delegated run stops to ask the person (D5a.2). It
// is also on ExecuteResult.InputRequired. Kernel-authored.
const HandoffKeyInputRequired = "_input_required"

// HandoffKeyAskBackRound is the request metadata key carrying the ask-back round
// a delegated request answers (1 for the answer to the first question set).
// Kernel-written only: Server.Execute stamps it from the caller's turn lease
// (LeaseBinding.AskBackRound, set by the chat lane; see HandoffKeyAskBackPendingRef)
// and deletes whatever a caller sent. The kernel stops asking once
// execution.plan.ask_back_max_rounds is reached and proceeds with stated
// assumptions (D5a.4). Absent ⇒ round 0.
const HandoffKeyAskBackRound = "_ask_back_round"

// InputRequired is the structured "input required" result of a delegated run
// (ADR-0145 D5a; precedent: the A2A INPUT_REQUIRED task state). The run stops,
// the chat agent asks the person, and the person's answer starts the next turn.
// The partial checklist with its item statuses (Requirements, the same shape as
// ExecuteResult.Requirements) and the questions are recorded in the run ledger
// (LedgerRef), so the next turn resumes from that state by reference.
type InputRequired struct {
	Version int    `json:"version"`
	RunID   string `json:"run_id,omitempty"`
	// Stage is the planning stage that asked (1 = before any work ran).
	Stage     int             `json:"stage"`
	Questions []InputQuestion `json:"questions"`
	// Reason is the planner's one-line why, for the trace.
	Reason string `json:"reason,omitempty"`
	// Round is this ask-back round for the request (1 = the first question set).
	Round int `json:"round"`
	// Requirements is the checklist and its coverage so far.
	Requirements json.RawMessage `json:"requirements,omitempty"`
	LedgerRef    string          `json:"ledger_ref,omitempty"`
}

// InputRequiredError is what the planner returns when its reply is a question for
// the person instead of a stage. The server decides what happens to it: ask on
// the conversation path, proceed with assumptions on every other path (D5a.2-3).
type InputRequiredError struct {
	Questions []InputQuestion
	Reason    string
}

func (e *InputRequiredError) Error() string {
	qs := make([]string, 0, len(e.Questions))
	for _, q := range e.Questions {
		qs = append(qs, q.Question)
	}
	return fmt.Sprintf("planner: input required (%d questions): %s", len(e.Questions), strings.Join(qs, " | "))
}

// ErrNoFurtherStage is what the planner returns when its reply is "nothing more
// to add". It is not "done": code decides that; the open items close as gaps.
var ErrNoFurtherStage = errors.New("planner: nothing more to plan for the open items")

// LedgerLookup is one read the planner asks for between its calls: a ledger
// kernel tool (ledger_get or ledger_find) and its arguments, exactly as a worker
// would call it.
type LedgerLookup struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// LedgerLookupFunc serves one LedgerLookup under the caller's read predicate and
// returns what the planner is shown (the tool's JSON result or its error text).
type LedgerLookupFunc func(ctx context.Context, l LedgerLookup) string

// StagePlanning is what a staged planner call is told beyond the checklist
// (carried on ctx by the server; the checklist and the open items ride
// WithRequirementChecklist / WithCoverageStage as before). Its presence selects
// the staged prompt arm.
type StagePlanning struct {
	// Number is the stage being planned (1 = the first).
	Number int
	// LedgerIndex is the compact index of the run's (or turn's) ledger entries:
	// one line per entry, id, kind, tool, step and a short preview.
	LedgerIndex string
	// EarlierStages lists the earlier stages' step results as references.
	EarlierStages string
	// CanAsk is true when a question can reach the person (the conversation path
	// with ask-back on and rounds left). When false the planner must not ask: it
	// proceeds and states its assumptions.
	CanAsk bool
	// Lookup serves the planner's ledger reads; nil ⇒ the planner cannot read.
	Lookup LedgerLookupFunc
	// MaxLookupRounds bounds the read rounds of one planner call (a runaway guard,
	// execution.plan.stage_ledger_read_rounds).
	MaxLookupRounds int

	mu          sync.Mutex
	assumptions []string
}

// AddAssumptions records the assumptions the planner stated for this stage (it
// proceeded without asking). The server writes them into the checklist.
func (s *StagePlanning) AddAssumptions(as []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range as {
		if a = strings.TrimSpace(a); a != "" {
			s.assumptions = append(s.assumptions, a)
		}
	}
}

// Assumptions returns the recorded assumptions.
func (s *StagePlanning) Assumptions() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.assumptions...)
}

type stagePlanningKey struct{}

// WithStagePlanning carries s on ctx for the planner.
func WithStagePlanning(ctx context.Context, s *StagePlanning) context.Context {
	return context.WithValue(ctx, stagePlanningKey{}, s)
}

// StagePlanningFrom returns the staged-planning context on ctx, or nil (the
// single-shot or coverage-stage planner).
func StagePlanningFrom(ctx context.Context) *StagePlanning {
	s, _ := ctx.Value(stagePlanningKey{}).(*StagePlanning)
	return s
}

// ─── Plan generations (T-20261108-ledger-step-indices-collide-across-replans-in) ──

// A replan inside one Execute swaps in a new plan whose step indices start again
// at 0 under the same run id. Ledger provenance is (run, step), so without a
// generation the new plan's step N would see the swapped-out plan's step N tool
// results as its own. Every plan generation after the first therefore records
// under its own ledger run id, exactly as a coverage stage does (<run>-s<k>).

type planGenerationKey struct{}

// WithPlanGeneration carries the executor's plan generation (0 = the plan it
// started with; +1 per replan, hot swap or injection) on a step's ctx.
func WithPlanGeneration(ctx context.Context, g int) context.Context {
	return context.WithValue(ctx, planGenerationKey{}, g)
}

// PlanGenerationFrom returns the plan generation on ctx (0 when absent).
func PlanGenerationFrom(ctx context.Context) int {
	g, _ := ctx.Value(planGenerationKey{}).(int)
	return g
}

// LedgerRunID is the run id a plan generation's ledger entries are recorded
// under: the run itself for generation 0, "<run>-g<n>" after a replan.
func LedgerRunID(run string, generation int) string {
	if generation <= 0 || run == "" {
		return run
	}
	return fmt.Sprintf("%s-g%d", run, generation)
}

// RootRunID is the run an id belongs to with its coverage-stage ("-s<n>") and
// plan-generation ("-g<n>") suffixes removed: every stage and replan of one
// Execute share it. Run ids are hex (newPlanID), so the first "-s"/"-g" is the
// suffix.
func RootRunID(run string) string {
	if i := strings.Index(run, "-s"); i > 0 {
		run = run[:i]
	}
	if i := strings.Index(run, "-g"); i > 0 {
		run = run[:i]
	}
	return run
}
