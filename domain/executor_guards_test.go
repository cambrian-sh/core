package domain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ADR-0145 D9 executor guards (T-20261108-adr-0145-phase-2-requirement-checklist-with);
// the D14 request-tied guards are in request_guard_test.go.

type countingHandler struct {
	n      atomic.Int32
	result []byte
	err    error
}

func (h *countingHandler) Execute(context.Context, ToolCall) ([]byte, error) {
	h.n.Add(1)
	return h.result, h.err
}

func guardedExec(t *testing.T, h ToolHandler) *ToolExecutor {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "crm_search", Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: "mail_draft", Effects: []ToolEffect{EffectWrite}})
	reg.Register(SystemTool{Name: "mail_send", Effects: []ToolEffect{EffectEgress}})
	return &ToolExecutor{Registry: reg, Handler: h, InlineThreshold: 1 << 20, unrestricted: true,
		idempotency: NewIdempotencyCache(0)}
}

func stepCtx(g StepGuard) context.Context {
	return WithStepCall(context.Background(), StepCall{RunID: "run-1", StepIndex: 2, Guard: g})
}

// ADR-0145 D14 removed the per-step effect menu: whatever a step "declared", a
// granted tool is callable; only the person's access, approvals and the
// request-tied guards decide.
func TestNoPerStepEffectMenu(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"MSG-1"}`)}
	e := guardedExec(t, h)
	for _, tool := range []string{"crm_search", "mail_draft", "mail_send"} {
		if got := e.Execute(stepCtx(StepGuard{}), ToolCallRequest{AgentID: "worker", ToolName: tool, ArgsJSON: []byte(`{}`)}); got.Denied {
			t.Fatalf("%s refused without any request guard: %+v", tool, got)
		}
	}
}

// A retried side-effecting call (same run, step, tool and canonical arguments)
// returns the stored result; the write runs once. Key order in the arguments does
// not make a new call; a different step or different arguments do.
func TestIdempotency_ADuplicatedWriteReturnsTheStoredResult(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"MSG-7"}`)}
	e := guardedExec(t, h)
	ctx := stepCtx(StepGuard{})
	first := e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":["a@x"],"subject":"Reminder"}`)})
	again := e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"subject":"Reminder","to":["a@x"]}`)})
	if h.n.Load() != 1 || string(again.ResultJSON) != string(first.ResultJSON) {
		t.Fatalf("duplicate write ran %d times; first %s again %s", h.n.Load(), first.ResultJSON, again.ResultJSON)
	}
	e.Execute(WithStepCall(context.Background(), StepCall{RunID: "run-1", StepIndex: 3}),
		ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":["a@x"],"subject":"Reminder"}`)})
	e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":["b@x"],"subject":"Reminder"}`)})
	if h.n.Load() != 3 {
		t.Fatalf("another step or other arguments are a new call: ran %d", h.n.Load())
	}
	// A read is never keyed.
	e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "crm_search", ArgsJSON: []byte(`{}`)})
	e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "crm_search", ArgsJSON: []byte(`{}`)})
	if h.n.Load() != 5 {
		t.Fatalf("reads must not be deduplicated: ran %d", h.n.Load())
	}
}

// A failed write is not stored: the retry runs again. Concurrent duplicates wait
// for the first and share its result.
func TestIdempotency_FailuresAreRetriedAndConcurrentDuplicatesWait(t *testing.T) {
	h := &countingHandler{err: errors.New("smtp down")}
	e := guardedExec(t, h)
	ctx := stepCtx(StepGuard{})
	call := ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":["a@x"]}`)}
	e.Execute(ctx, call)
	e.Execute(ctx, call)
	if h.n.Load() != 2 {
		t.Fatalf("a failed write must be retryable, ran %d", h.n.Load())
	}
	h2 := &countingHandler{result: []byte(`{"id":"MSG-9"}`)}
	e2 := guardedExec(t, h2)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e2.Execute(ctx, call); string(r.ResultJSON) != `{"id":"MSG-9"}` {
				t.Errorf("result %+v", r)
			}
		}()
	}
	wg.Wait()
	if h2.n.Load() != 1 {
		t.Fatalf("8 concurrent duplicates ran the write %d times", h2.n.Load())
	}
}

type rejectArgs struct{}

func (rejectArgs) ValidateArgs(tool string, _, args []byte) ([]byte, error) {
	if strings.Contains(string(args), "bad") {
		return nil, errors.New("arguments for " + tool + " do not match its schema: /to: want array")
	}
	return []byte(`{"to":["fixed@x"]}`), nil
}

// The validator runs before anything else sees the arguments: an invalid call is
// an error value the model can correct and never runs; a coerced call runs with
// the coerced arguments.
func TestArgValidator_InvalidNeverRunsCoercedRunsFixed(t *testing.T) {
	h := &fakeHandler{result: []byte(`{}`)}
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "mail_draft", Effects: []ToolEffect{EffectWrite}, Schema: []byte(`{"type":"object"}`)})
	e := &ToolExecutor{Registry: reg, Handler: h, InlineThreshold: 1 << 20, unrestricted: true, argValidator: rejectArgs{}}
	got := e.Execute(context.Background(), ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":"bad"}`)})
	if got.Error == "" || got.Denied || h.called {
		t.Fatalf("invalid args: %+v (ran %v)", got, h.called)
	}
	got = e.Execute(context.Background(), ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: []byte(`{"to":"[\"fixed@x\"]"}`)})
	if got.Error != "" || string(h.gotArgs) != `{"to":["fixed@x"]}` {
		t.Fatalf("coerced args not used: %+v, ran with %s", got, h.gotArgs)
	}
}
