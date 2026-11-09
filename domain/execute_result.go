package domain

import "encoding/json"

// HandoffKeyExecuteResult is the Execute response metadata key that carries the
// structured result (ADR-0145 D8): ExecuteResult as JSON. The payload keeps the
// report text for callers that read only text.
const HandoffKeyExecuteResult = "_execute_result"

// ExecuteResultVersion is the shape version of ExecuteResult.
const ExecuteResultVersion = 1

// ExecuteResult is what one Execute produced, delivered whole to the caller
// (ADR-0145 D8 "structured result"; the SDK's execute() returns all of it). It
// replaces "the sink steps' prose" as the only thing a caller learns: every step
// is listed with its status and, when the run ledger is on, the ledger reference
// of its full output, so the caller (the chat agent, a composer) can read any
// step's result — not only the ones the report happened to keep.
type ExecuteResult struct {
	Version   int    `json:"version"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Report is the same text as the response payload.
	Report string `json:"report"`
	// Actions is the plan's action ledger: every side-effecting tool call its
	// steps made, with status and the ids it returned.
	Actions []ExecuteAction `json:"actions"`
	// Steps lists every step of the final plan (after replans and fan-out).
	Steps []ExecuteStepRef `json:"steps"`
	// Partial is set when the plan stopped before every step completed.
	Partial bool `json:"partial,omitempty"`
	// TimedOut is set when a plan limit (idle, max duration, the caller-deadline
	// reserve) ended it; TimeoutCause names which.
	TimedOut     bool   `json:"timed_out,omitempty"`
	TimeoutCause string `json:"timeout_cause,omitempty"`
	// FailedStep is the step that failed, -1 when none did.
	FailedStep int    `json:"failed_step"`
	Error      string `json:"error,omitempty"`
	// Ledger reports whether Steps[].Ref are ledger references (execution.ledger.enabled).
	Ledger bool `json:"ledger"`
	// Requirements is the request's requirement checklist and its code-decided
	// coverage (ADR-0145 D5: every item's status, accepted values with their
	// evidence refs, reasons and gaps), when execution.plan.checklist_enabled is on.
	Requirements json.RawMessage `json:"requirements,omitempty"`
	// InputRequired is set when the run stopped to ask the person (ADR-0145 D5a,
	// execution.plan.ask_back_enabled on the conversation path): the questions,
	// the stage that asked and the partial coverage. The payload then carries the
	// questions as text. Also on metadata under HandoffKeyInputRequired.
	InputRequired *InputRequired `json:"input_required,omitempty"`
}

// ExecuteAction is one entry of the plan's action ledger (PlanAction, wire form).
type ExecuteAction struct {
	Step   int               `json:"step"`
	Agent  string            `json:"agent,omitempty"`
	Tool   string            `json:"tool"`
	Status string            `json:"status"`
	Detail string            `json:"detail,omitempty"`
	IDs    []string          `json:"ids,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
}

// Step statuses in ExecuteStepRef.
const (
	ExecuteStepCompleted = "completed"
	ExecuteStepEmpty     = "empty"
	ExecuteStepNotRun    = "not_run"
)

// ExecuteStepRef is one step of the run in the structured result.
type ExecuteStepRef struct {
	Step   int    `json:"step"`
	Query  string `json:"query,omitempty"`
	Status string `json:"status"`
	// Ref is the step output's ledger id (readable with ledger_get or
	// GetContextNode) when the ledger is on; "" otherwise.
	Ref     string `json:"ref,omitempty"`
	Preview string `json:"preview,omitempty"`
	Bytes   int    `json:"bytes"`
	// FanOutItem is the item a fan-out child ran for.
	FanOutItem string `json:"fan_out_item,omitempty"`
	Thought    bool   `json:"thought,omitempty"`
	// Stage is the ADR-0145 coverage stage the step ran in (1 = the first plan;
	// 0 when the checklist is off). Step indices restart in every stage.
	Stage int `json:"stage,omitempty"`
}

// ExecuteActionsOf converts the plan action ledger to its wire form.
func ExecuteActionsOf(actions []PlanAction) []ExecuteAction {
	out := make([]ExecuteAction, 0, len(actions))
	for _, a := range actions {
		out = append(out, ExecuteAction{Step: a.Step, Agent: a.Agent, Tool: a.Tool, Status: a.Status,
			Detail: a.Detail, IDs: a.IDs, Args: a.Args})
	}
	return out
}
