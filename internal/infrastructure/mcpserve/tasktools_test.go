package mcpserve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// fakeTaskLane records what the tools hand it and serves canned views.
type fakeTaskLane struct {
	submitted   []fakeSubmission
	submitErr   error
	views       map[string]TaskView
	nextID      string
	statusCalls []string
}

type fakeSubmission struct {
	task, parent string
	beneficiary  domain.PrincipalRef
}

func (f *fakeTaskLane) Submit(_ context.Context, task, parentID string, beneficiary domain.PrincipalRef) (string, error) {
	f.submitted = append(f.submitted, fakeSubmission{task: task, parent: parentID, beneficiary: beneficiary})
	if f.submitErr != nil {
		return "", f.submitErr
	}
	if f.nextID == "" {
		return "task-1", nil
	}
	return f.nextID, nil
}

func (f *fakeTaskLane) Status(_ context.Context, taskID string) (TaskView, bool) {
	f.statusCalls = append(f.statusCalls, taskID)
	v, ok := f.views[taskID]
	return v, ok
}

func taskHandlers(t *testing.T, lane TaskLane) (submit, status domain.PublishedToolHandler) {
	t.Helper()
	for _, e := range TaskTools(lane) {
		switch e.Tool.Name {
		case "submit_task":
			submit = e.Handler
		case "get_task_status":
			status = e.Handler
		}
	}
	if submit == nil || status == nil {
		t.Fatal("TaskTools did not publish both submit_task and get_task_status")
	}
	return submit, status
}

func alice() context.Context {
	return domain.WithPrincipal(context.Background(), domain.AgentPrincipal("mcp:alice"))
}

// The declarations are public contract: names in the D7 grammar, honest
// effects (submission writes state and spends budget; status is a read), and
// NEITHER is machine-only — the task lane is for every authenticated caller,
// unlike the worker transport beside it.
func TestTaskTools_Declarations(t *testing.T) {
	surface := TaskTools(&fakeTaskLane{})
	if len(surface) != 2 {
		t.Fatalf("task tools = %d, want 2", len(surface))
	}
	for _, e := range surface {
		if !domain.ValidPublishedToolName(e.Tool.Name) {
			t.Errorf("%s: name violates the D7 grammar", e.Tool.Name)
		}
		if e.Tool.MachineOnly {
			t.Errorf("%s: must not be machine-only", e.Tool.Name)
		}
		var probe map[string]any
		if err := json.Unmarshal(e.Tool.InputSchema, &probe); err != nil || probe["type"] != "object" {
			t.Errorf("%s: input schema is not an object schema (%v)", e.Tool.Name, err)
		}
		switch e.Tool.Name {
		case "submit_task":
			if e.Tool.ReadOnly {
				t.Error("submit_task: marked read-only, but it mutates and spends")
			}
			want := map[domain.ToolEffect]bool{domain.EffectWrite: true, domain.EffectSpend: true}
			for _, eff := range e.Tool.Effects {
				delete(want, eff)
			}
			if len(want) != 0 {
				t.Errorf("submit_task: effects %v missing from %v", want, e.Tool.Effects)
			}
		case "get_task_status":
			if !e.Tool.ReadOnly {
				t.Error("get_task_status: not marked read-only")
			}
			if len(e.Tool.Effects) != 1 || e.Tool.Effects[0] != domain.EffectRead {
				t.Errorf("get_task_status: effects = %v, want [read]", e.Tool.Effects)
			}
		}
	}
}

// Fail-closed identity: an anonymous caller can neither submit nor poll —
// whatever the (fail-open OSS) authorizer would say, the lane itself refuses.
func TestTaskTools_AnonymousCallerIsRefused(t *testing.T) {
	lane := &fakeTaskLane{}
	submit, status := taskHandlers(t, lane)

	if _, err := submit.Invoke(context.Background(), json.RawMessage(`{"task":"do the thing"}`)); err == nil {
		t.Fatal("anonymous submit_task was accepted")
	}
	if _, err := status.Invoke(context.Background(), json.RawMessage(`{"task_id":"task-1"}`)); err == nil {
		t.Fatal("anonymous get_task_status was answered")
	}
	if len(lane.submitted) != 0 {
		t.Fatal("an anonymous submission reached the lane")
	}
}

// A machine principal with no owner on the context is a wiring defect and must
// refuse — a task nobody is the beneficiary of must not exist.
func TestTaskTools_OwnerlessMachineIsRefused(t *testing.T) {
	lane := &fakeTaskLane{}
	submit, _ := taskHandlers(t, lane)
	ctx := domain.WithPrincipal(context.Background(), domain.MachinePrincipal("laptop"))
	if _, err := submit.Invoke(ctx, json.RawMessage(`{"task":"x"}`)); err == nil {
		t.Fatal("ownerless machine principal was allowed to submit")
	}
}

// The beneficiary derivation, both shapes: an ordinary principal is its own
// beneficiary; a worker machine submits FOR ITS OWNER (the ADR-0127 D1 binding
// carried by the middleware) — never for itself.
func TestSubmitTask_BeneficiaryDerivation(t *testing.T) {
	lane := &fakeTaskLane{}
	submit, _ := taskHandlers(t, lane)

	if _, err := submit.Invoke(alice(), json.RawMessage(`{"task":"inventory the repo"}`)); err != nil {
		t.Fatalf("submit as mcp:alice: %v", err)
	}
	machineCtx := domain.WithWorkerOwner(
		domain.WithPrincipal(context.Background(), domain.MachinePrincipal("laptop")),
		domain.AgentPrincipal("mcp:alice"))
	if _, err := submit.Invoke(machineCtx, json.RawMessage(`{"task":"sync my notes"}`)); err != nil {
		t.Fatalf("submit as machine:laptop: %v", err)
	}

	if len(lane.submitted) != 2 {
		t.Fatalf("submissions = %d, want 2", len(lane.submitted))
	}
	want := domain.AgentPrincipal("mcp:alice")
	if lane.submitted[0].beneficiary != want {
		t.Errorf("ordinary caller: beneficiary = %+v, want itself (%+v)", lane.submitted[0].beneficiary, want)
	}
	if lane.submitted[1].beneficiary != want {
		t.Errorf("machine caller: beneficiary = %+v, want its OWNER (%+v)", lane.submitted[1].beneficiary, want)
	}
}

func TestSubmitTask_RequiresTaskText(t *testing.T) {
	lane := &fakeTaskLane{}
	submit, _ := taskHandlers(t, lane)
	for _, args := range []string{`{}`, `{"task":"   "}`} {
		if _, err := submit.Invoke(alice(), json.RawMessage(args)); err == nil {
			t.Errorf("args %s: accepted without a task", args)
		}
	}
	if len(lane.submitted) != 0 {
		t.Fatal("an empty task reached the lane")
	}
}

// The answer carries the task id and the initial state, so a caller can start
// polling without guessing.
func TestSubmitTask_AnswersWithTaskID(t *testing.T) {
	lane := &fakeTaskLane{nextID: "ab12cd34"}
	submit, _ := taskHandlers(t, lane)
	res, err := submit.Invoke(alice(), json.RawMessage(`{"task":"do the thing"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	structured, ok := res.Structured.(map[string]any)
	if !ok || structured["task_id"] != "ab12cd34" || structured["state"] != TaskStateRunning {
		t.Fatalf("structured = %#v, want task_id ab12cd34 in state running", res.Structured)
	}
	if !strings.Contains(res.Text, "ab12cd34") {
		t.Fatalf("text %q does not name the task id", res.Text)
	}
}

// No task-id enumeration: a nonexistent id and another principal's id answer
// with BYTE-IDENTICAL errors, from submit_task's parent check and from
// get_task_status alike.
func TestTaskStatus_UnknownAndForeignAnswerIdentically(t *testing.T) {
	lane := &fakeTaskLane{views: map[string]TaskView{
		"bobs-task": {ID: "bobs-task", State: TaskStateRunning, Beneficiary: domain.AgentPrincipal("mcp:bob")},
	}}
	_, status := taskHandlers(t, lane)

	_, unknownErr := status.Invoke(alice(), json.RawMessage(`{"task_id":"never-existed"}`))
	_, foreignErr := status.Invoke(alice(), json.RawMessage(`{"task_id":"bobs-task"}`))
	if unknownErr == nil || foreignErr == nil {
		t.Fatal("unknown or foreign task id was answered")
	}
	if unknownErr.Error() != foreignErr.Error() {
		t.Fatalf("enumeration channel: unknown answers %q, foreign answers %q", unknownErr, foreignErr)
	}
}

// A parent must be one of the caller's own tasks — with the same not-found
// shape as the status read, for the same anti-enumeration reason.
func TestSubmitTask_ParentMustBeCallersOwnTask(t *testing.T) {
	lane := &fakeTaskLane{views: map[string]TaskView{
		"bobs-task":   {ID: "bobs-task", State: TaskStateRunning, Beneficiary: domain.AgentPrincipal("mcp:bob")},
		"alices-task": {ID: "alices-task", State: TaskStateSucceeded, Beneficiary: domain.AgentPrincipal("mcp:alice")},
	}}
	submit, _ := taskHandlers(t, lane)

	if _, err := submit.Invoke(alice(), json.RawMessage(`{"task":"x","parent_task_id":"bobs-task"}`)); err == nil {
		t.Fatal("a foreign parent task id was accepted")
	}
	if _, err := submit.Invoke(alice(), json.RawMessage(`{"task":"x","parent_task_id":"missing"}`)); err == nil {
		t.Fatal("a nonexistent parent task id was accepted")
	}
	if len(lane.submitted) != 0 {
		t.Fatal("a submission with an invisible parent reached the lane")
	}
	if _, err := submit.Invoke(alice(), json.RawMessage(`{"task":"x","parent_task_id":"alices-task"}`)); err != nil {
		t.Fatalf("the caller's own parent task was refused: %v", err)
	}
}

// The status answer: state, goal, terminality, timestamps — and the result
// once the task is terminal. The beneficiary itself is never serialized.
func TestTaskStatus_AnswersOwnTask(t *testing.T) {
	created := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	lane := &fakeTaskLane{views: map[string]TaskView{
		"t1": {
			ID: "t1", Goal: "inventory the repo", State: TaskStateSucceeded,
			Result: "an inventory, with citations", Beneficiary: domain.AgentPrincipal("mcp:alice"),
			CreatedAt: created, UpdatedAt: created.Add(time.Minute),
		},
	}}
	_, status := taskHandlers(t, lane)

	res, err := status.Invoke(alice(), json.RawMessage(`{"task_id":"t1"}`))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	structured := res.Structured.(map[string]any)
	if structured["state"] != TaskStateSucceeded || structured["terminal"] != true {
		t.Fatalf("structured = %#v, want terminal succeeded", structured)
	}
	if structured["result"] != "an inventory, with citations" {
		t.Fatalf("result missing: %#v", structured)
	}
	if _, leaked := structured["beneficiary"]; leaked {
		t.Fatal("the beneficiary principal is serialized to the caller")
	}
	if !strings.Contains(res.Text, TaskStateSucceeded) {
		t.Fatalf("text %q does not carry the state", res.Text)
	}
}

// The pair renders over the real endpoint: the SDK accepts both declarations
// and an ordinary (non-machine) client is shown them — they are product
// surface, unlike the machine-only worker transport beside them.
func TestTaskTools_ListedOverTheEndpoint(t *testing.T) {
	session := serve(t, Options{Surface: TaskTools(&fakeTaskLane{})}, "token-aaaa")
	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	seen := map[string]bool{}
	for _, tool := range res.Tools {
		seen[tool.Name] = true
	}
	if !seen["submit_task"] || !seen["get_task_status"] {
		t.Fatalf("tools/list = %v, want submit_task and get_task_status listed", seen)
	}
}

// A machine principal polls its OWNER's tasks — the same derivation as submit.
func TestTaskStatus_MachineSeesItsOwnersTasks(t *testing.T) {
	lane := &fakeTaskLane{views: map[string]TaskView{
		"t1": {ID: "t1", State: TaskStateRunning, Beneficiary: domain.AgentPrincipal("mcp:alice")},
	}}
	_, status := taskHandlers(t, lane)
	machineCtx := domain.WithWorkerOwner(
		domain.WithPrincipal(context.Background(), domain.MachinePrincipal("laptop")),
		domain.AgentPrincipal("mcp:alice"))
	if _, err := status.Invoke(machineCtx, json.RawMessage(`{"task_id":"t1"}`)); err != nil {
		t.Fatalf("machine could not poll its owner's task: %v", err)
	}
}
