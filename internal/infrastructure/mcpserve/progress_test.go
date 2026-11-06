package mcpserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 amendment 2026-11-06: a task call that carries a progressToken is held
// open while the task runs, and the task's live step output arrives as
// notifications/progress — over the real stateless streamable-HTTP endpoint.

// liveLane is a task lane whose task finishes after a few status polls.
type liveLane struct {
	mu          sync.Mutex
	beneficiary domain.PrincipalRef
	polls       int
	doneAfter   int
}

func (l *liveLane) Submit(_ context.Context, _, _ string, b domain.PrincipalRef) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.beneficiary = b
	return "task-1", nil
}

func (l *liveLane) Status(_ context.Context, id string) (TaskView, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != "task-1" || l.beneficiary.IsZero() {
		return TaskView{}, false
	}
	l.polls++
	v := TaskView{ID: id, State: TaskStateRunning, Beneficiary: l.beneficiary}
	if l.polls > l.doneAfter {
		v.State, v.Result = TaskStateSucceeded, "the answer"
	}
	return v, true
}

// scriptedOutput is a TaskOutput whose subscription immediately replays chunks
// (synchronously, as the real stream delivers).
func scriptedOutput(chunks ...domain.StepOutputChunk) (TaskOutput, *[]string) {
	var subscribed []string
	return TaskOutput{
		Subscribe: func(taskID string, fn func(domain.StepOutputChunk)) func() {
			subscribed = append(subscribed, taskID)
			for _, c := range chunks {
				fn(c)
			}
			return func() {}
		},
		DefaultWait: 5 * time.Second,
		MaxWait:     10 * time.Second,
	}, &subscribed
}

// serveWithProgress is serve() with a client that records progress messages.
func serveWithProgress(t *testing.T, opts Options) (*mcpsdk.ClientSession, func() []string) {
	t.Helper()
	opts.Clients = []string{"ci-bot"}
	opts.ResolveSecret = staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"})
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	var mu sync.Mutex
	var msgs []string
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "progress-client", Version: "1.0.0"},
		&mcpsdk.ClientOptions{ProgressNotificationHandler: func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			msgs = append(msgs, req.Params.Message)
		}})
	session, err := client.Connect(t.Context(), &mcpsdk.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           &http.Client{Transport: bearerRoundTripper{token: "token-aaaa", base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), msgs...)
	}
}

func TestSubmitTask_ProgressTokenStreamsStepOutputAsProgress(t *testing.T) {
	t.Parallel()
	lane := &liveLane{doneAfter: 2}
	out, subscribed := scriptedOutput(
		domain.StepOutputChunk{SessionID: "task-1", Kind: domain.StepOutputText, Data: "Hel"},
		domain.StepOutputChunk{SessionID: "task-1", Kind: domain.StepOutputText, Data: "lo"},
		domain.StepOutputChunk{SessionID: "task-1", Kind: domain.StepOutputProgress, Data: "step 2 of 2"},
	)
	session, progress := serveWithProgress(t, Options{Surface: TaskTools(lane, out)})

	params := &mcpsdk.CallToolParams{Name: "submit_task", Arguments: map[string]any{"task": "say hello"}}
	params.SetProgressToken("tok-1")
	res, err := session.CallTool(t.Context(), params)
	if err != nil || res.IsError {
		t.Fatalf("submit_task: %v %+v", err, res)
	}
	if len(*subscribed) != 1 || (*subscribed)[0] != "task-1" {
		t.Fatalf("subscribed to %v", *subscribed)
	}
	got := progress()
	if strings.Join(got, "|") != "Hello|step 2 of 2" {
		t.Fatalf("progress messages = %q; want the coalesced text, then the progress note", got)
	}
	// The call answered with the state it reached, not the bare "accepted".
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"state":"succeeded"`) || !strings.Contains(string(raw), "the answer") {
		t.Fatalf("structured = %s", raw)
	}
}

func TestSubmitTask_WithoutProgressTokenReturnsAtOnce(t *testing.T) {
	t.Parallel()
	lane := &liveLane{doneAfter: 1000}
	out, subscribed := scriptedOutput(domain.StepOutputChunk{Data: "x"})
	session, progress := serveWithProgress(t, Options{Surface: TaskTools(lane, out)})
	start := time.Now()
	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "submit_task", Arguments: map[string]any{"task": "t"}})
	if err != nil || res.IsError {
		t.Fatalf("submit_task: %v %+v", err, res)
	}
	if time.Since(start) > 2*time.Second || len(*subscribed) != 0 || len(progress()) != 0 {
		t.Fatalf("a call with no progressToken must not wait or stream (subscribed %v, progress %v)", *subscribed, progress())
	}
}

func TestGetTaskStatus_WaitIsBoundedAndVisibilityComesFirst(t *testing.T) {
	t.Parallel()
	lane := &liveLane{doneAfter: 1000} // never finishes
	out, subscribed := scriptedOutput()
	out.MaxWait = 700 * time.Millisecond
	surface := TaskTools(lane, out)
	var get domain.PublishedToolHandler
	var submit domain.PublishedToolHandler
	for _, e := range surface {
		switch e.Tool.Name {
		case "get_task_status":
			get = e.Handler
		case "submit_task":
			submit = e.Handler
		}
	}
	if _, err := submit.Invoke(alice(), json.RawMessage(`{"task":"t"}`)); err != nil {
		t.Fatal(err)
	}
	// Someone else cannot long-poll (or subscribe to) alice's task.
	bob := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("mcp:bob"))
	if _, err := get.Invoke(bob, json.RawMessage(`{"task_id":"task-1","wait_seconds":5}`)); err == nil {
		t.Fatal("bob saw alice's task")
	}
	if len(*subscribed) != 0 {
		t.Fatal("a refused caller was subscribed to the task's output")
	}
	start := time.Now()
	res, err := get.Invoke(alice(), json.RawMessage(`{"task_id":"task-1","wait_seconds":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 600*time.Millisecond || d > 3*time.Second {
		t.Fatalf("wait took %v; want it bounded by MaxWait (700ms)", d)
	}
	if st, _ := res.Structured.(map[string]any); st["state"] != TaskStateRunning {
		t.Fatalf("structured = %v", res.Structured)
	}
}
