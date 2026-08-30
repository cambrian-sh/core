package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The task hand-off pair (ADR-0126 D10, phase 4): `submit_task` and
// `get_task_status`.
//
// Submission enters the kernel's EXISTING task lane — the same
// session + Execute path an operator-submitted task takes (owner
// course-correction over the ADR's earlier reactive-pipeline-head sketch; the
// implementation record in ADR-0126 carries the deviation). There is no second
// task engine, no parallel queue and no new state store behind these tools:
// the lane opens a task session, dispatches through the one Execute entrypoint,
// and remembers the outcome in the kernel's memory-resident task index — the
// same residence as the execution itself, which does not survive a restart
// either.
//
// The load-bearing half is the BENEFICIARY: every submitted task records the
// owner principal it is being done FOR, derived from the authenticated caller
// and never from an argument (INV-5). That principal is what
// domain.WithTaskBeneficiary carries to menu building, which is how a
// submitter's live worker fleet (ADR-0127 D4) reaches the agents attending
// exactly that submitter's tasks and nobody else's.
//
// These are deliberately NOT in CoreTools: the golden contract test freezes
// the four read-only memory tools alone, and the task pair rides kernel
// handles (session manager, Execute) that only the composition root holds —
// the same reason ContributionLaneTools composes beside CoreTools rather than
// inside it.

// Task states get_task_status reports. The vocabulary is the internal lane's
// own, projected: a task session is active (running), paused or dormant (the
// operator's steering states), or terminal — succeeded when its plan finished,
// failed when dispatch or a step did not.
const (
	TaskStateRunning   = "running"
	TaskStatePaused    = "paused"
	TaskStateDormant   = "dormant"
	TaskStateSucceeded = "succeeded"
	TaskStateFailed    = "failed"
)

// TaskTerminal reports whether a state is final — polling past it changes
// nothing.
func TaskTerminal(state string) bool {
	return state == TaskStateSucceeded || state == TaskStateFailed
}

// TaskView is one task as the status tool renders it. Beneficiary is carried
// for the visibility check and never serialized to the caller.
type TaskView struct {
	ID          string
	Goal        string
	State       string
	Result      string
	Beneficiary domain.PrincipalRef
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskLane is the seam the composition root binds these tools to (the same
// shape CoreBackends gives the memory tools). Its implementation reuses the
// kernel's existing task machinery — session manager + the Execute entrypoint
// — and owns the memory-resident task index.
type TaskLane interface {
	// Submit opens a task session for beneficiary and dispatches the task text
	// into the kernel's execute lane, returning the task id. The beneficiary is
	// established by the TRANSPORT (taskCaller), never parsed from arguments.
	Submit(ctx context.Context, task, parentID string, beneficiary domain.PrincipalRef) (string, error)
	// Status reads one task. ok=false for an id this kernel is not tracking —
	// the caller-facing tool folds "unknown" and "not yours" into one answer.
	Status(ctx context.Context, taskID string) (TaskView, bool)
}

// taskTextMaxChars bounds one submission's text. Generous — a task is a
// natural-language ask, not a corpus upload — and a bound, because the text
// goes verbatim into the planner's prompt.
const taskTextMaxChars = 100_000

// errTaskNotVisible answers BOTH a nonexistent id and someone else's id, in
// exactly these bytes: two different answers would let a caller enumerate
// which task ids exist on this kernel.
var errTaskNotVisible = errors.New("no task with that id is visible to you")

// TaskTools renders the task hand-off pair over lane. Owner "core" beside the
// memory tools; composed by the root, never part of the CoreTools golden.
func TaskTools(lane TaskLane) domain.PublishedToolSurface {
	return domain.PublishedToolSurface{
		{Owner: "core", Tool: submitTaskTool, Handler: handlerFunc(func(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
			return submitTask(ctx, lane, args)
		})},
		{Owner: "core", Tool: getTaskStatusTool, Handler: handlerFunc(func(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
			return getTaskStatus(ctx, lane, args)
		})},
	}
}

// ── declarations ─────────────────────────────────────────────────────────────

var submitTaskTool = domain.PublishedTool{
	Name:  "submit_task",
	Title: "Submit a task",
	Description: "Hand Cambrian a task to accomplish, in natural language. It enters the " +
		"kernel's planner lane exactly as an operator-submitted task would and runs " +
		"asynchronously; the answer carries a task_id — poll get_task_status until the state " +
		"is succeeded or failed. Tasks run agents and spend LLM budget on your behalf.",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["task"],
  "properties": {
    "task": {"type": "string", "description": "What to accomplish, in natural language. Becomes the task's goal and the planner's input."},
    "parent_task_id": {"type": "string", "description": "Optional: a task id you submitted earlier, recording this task as its follow-up."}
  }
}`),
	// Honest effects: submission mutates kernel state (a task session, a plan,
	// its journal) and spends — the planner and every dispatched step run LLMs
	// on the deployment's budget.
	Effects: []domain.ToolEffect{domain.EffectWrite, domain.EffectSpend},
}

var getTaskStatusTool = domain.PublishedTool{
	Name:  "get_task_status",
	Title: "Get task status",
	Description: "Poll one of your submitted tasks by task_id. States: running, paused, " +
		"dormant, succeeded, failed — succeeded and failed are terminal (terminal: true), and " +
		"the result text arrives with them. Only tasks submitted under your own identity (or, " +
		"for a worker machine, its owner's) are visible; anything else answers not-found.",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["task_id"],
  "properties": {
    "task_id": {"type": "string", "description": "The id submit_task returned."}
  }
}`),
	Effects:  []domain.ToolEffect{domain.EffectRead},
	ReadOnly: true,
}

// ── handlers ─────────────────────────────────────────────────────────────────

// taskCaller derives the beneficiary owner principal from the authenticated
// context — the D4/INV-5 discipline: never from arguments.
//
//   - a worker machine acts for its OWNER (the durable binding made at token
//     issuance, carried by the middleware) — its tasks are its owner's tasks;
//   - a bound sender IS the resolved principal (the middleware already applied
//     the identity hop), so it acts for itself;
//   - an ordinary client principal acts for itself;
//   - no principal at all is refused: an anonymous caller must not be able to
//     submit work or probe task ids, whatever the OSS authorizer would allow.
func taskCaller(ctx context.Context) (domain.PrincipalRef, error) {
	if owner := domain.WorkerOwnerFromContext(ctx); !owner.IsZero() {
		return owner, nil
	}
	p := domain.PrincipalFromContext(ctx)
	if p.IsZero() {
		return domain.PrincipalRef{}, errors.New(
			"refused: the task lane requires an authenticated principal")
	}
	if p.Kind == domain.PrincipalMachine {
		// A machine principal with no owner on the context is a wiring defect
		// (the middleware only mints machine principals FROM an owner binding),
		// and a task nobody is the beneficiary of must not exist.
		return domain.PrincipalRef{}, errors.New(
			"refused: worker credential carries no owner principal; re-issue it with --owner")
	}
	return p, nil
}

func submitTask(ctx context.Context, lane TaskLane, args json.RawMessage) (domain.PublishedToolResult, error) {
	if lane == nil {
		return domain.PublishedToolResult{}, errors.New("the task lane is not available on this kernel")
	}
	owner, err := taskCaller(ctx)
	if err != nil {
		return domain.PublishedToolResult{}, err
	}
	var in struct {
		Task         string `json:"task"`
		ParentTaskID string `json:"parent_task_id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return domain.PublishedToolResult{}, fmt.Errorf("task is required")
	}
	if len([]rune(task)) > taskTextMaxChars {
		return domain.PublishedToolResult{}, fmt.Errorf(
			"task text exceeds %d characters; a task is an ask, not an upload — ingest large content instead", taskTextMaxChars)
	}
	// A parent must be one of the CALLER's own tasks. The same not-found shape
	// as get_task_status, for the same reason: a distinguishable refusal would
	// let a submitter probe which ids exist.
	if in.ParentTaskID != "" {
		parent, ok := lane.Status(ctx, in.ParentTaskID)
		if !ok || parent.Beneficiary != owner {
			return domain.PublishedToolResult{}, fmt.Errorf("parent_task_id: %w", errTaskNotVisible)
		}
	}
	id, err := lane.Submit(ctx, task, in.ParentTaskID, owner)
	if err != nil {
		return domain.PublishedToolResult{}, fmt.Errorf("submit: %w", err)
	}
	return domain.PublishedToolResult{
		Structured: map[string]any{"task_id": id, "state": TaskStateRunning},
		Text:       fmt.Sprintf("task %s accepted; poll get_task_status until it is terminal", id),
	}, nil
}

func getTaskStatus(ctx context.Context, lane TaskLane, args json.RawMessage) (domain.PublishedToolResult, error) {
	if lane == nil {
		return domain.PublishedToolResult{}, errors.New("the task lane is not available on this kernel")
	}
	owner, err := taskCaller(ctx)
	if err != nil {
		return domain.PublishedToolResult{}, err
	}
	var in struct {
		TaskID string `json:"task_id"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	if strings.TrimSpace(in.TaskID) == "" {
		return domain.PublishedToolResult{}, fmt.Errorf("task_id is required")
	}
	view, ok := lane.Status(ctx, in.TaskID)
	if !ok || view.Beneficiary != owner {
		// One answer for "no such task" and "not your task" — returning the
		// error VALUE both ways keeps the two byte-identical by construction.
		return domain.PublishedToolResult{}, errTaskNotVisible
	}
	structured := map[string]any{
		"task_id":            view.ID,
		"state":              view.State,
		"goal":               view.Goal,
		"terminal":           TaskTerminal(view.State),
		"created_at_unix_ms": view.CreatedAt.UnixMilli(),
		"updated_at_unix_ms": view.UpdatedAt.UnixMilli(),
	}
	text := fmt.Sprintf("task %s: %s", view.ID, view.State)
	if view.Result != "" {
		structured["result"] = view.Result
		text += "\n" + view.Result
	}
	return domain.PublishedToolResult{Structured: structured, Text: text}, nil
}
