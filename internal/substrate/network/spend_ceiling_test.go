package network

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// usageStreamer is a model that answers with fixed text and reports fixed
// provider usage on a TRAILING chunk after the final text chunk — the OpenAI
// shape, which the gateway used to cut off and lose.
type usageStreamer struct {
	text          string
	prompt, compl int
	cacheRead     int
}

func (u *usageStreamer) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 3)
	ch <- domain.StreamChunk{Text: u.text, IsFinal: true}
	if u.prompt+u.compl > 0 {
		ch <- domain.StreamChunk{IsFinal: true, UsagePromptTokens: u.prompt, UsageCompletionTokens: u.compl,
			UsageTotalTokens: u.prompt + u.compl, UsageCacheReadTokens: u.cacheRead}
	}
	close(ch)
	return ch, nil
}

func meteredGateway(meter *llm.SpendMeter, s domain.LLMStreamer) *SubstrateLLMGateway {
	cfg := config.DefaultConfig().Execution
	gw := NewLLMGateway(cfg)
	gw.RegisterModelClient("llm:fake", s)
	gw.SetDefaultModelID("llm:fake")
	gw.SetSpendMeter(meter)
	return gw
}

// streamingDispatcher plays the agent: it generates through the gateway under
// the lease the kernel injected, exactly as a cognitive agent's
// GenerateViaModelStream does.
type streamingDispatcher struct {
	gw    *SubstrateLLMGateway
	mu    sync.Mutex
	calls int
}

func (d *streamingDispatcher) Execute(ctx context.Context, _ *domain.DispatchTask, h *domain.Handoff) (*domain.DispatchResult, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	out := make(chan domain.StreamChunk, 16)
	if err := d.gw.StreamChunks(ctx, domain.LeaseID(h.Context["_session_token_id"]), "agent prompt", domain.GenerateOptions{}, out); err != nil {
		return nil, err
	}
	return &domain.DispatchResult{
		Handoff:    &domain.Handoff{FromAgent: "a", Payload: &domain.Payload{Data: []byte("step done")}},
		Confidence: 0.9,
	}, nil
}

func (d *streamingDispatcher) CallAgent(context.Context, string, *domain.Handoff, string) (*domain.Handoff, error) {
	return nil, errors.New("unused")
}

type capturedEvents struct {
	mu     sync.Mutex
	events []domain.TaskEvent
}

func (c *capturedEvents) Wrap(domain.TaskEventWriter) domain.TaskEventReadWriter { return c }
func (c *capturedEvents) WriteTaskEvent(e domain.TaskEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}
func (c *capturedEvents) ReadTaskEvent(string) (*domain.TaskEvent, error) { return nil, nil }

func ceilingServer(t *testing.T, meter *llm.SpendMeter, gw *SubstrateLLMGateway, steps int) (*Server, *streamingDispatcher, *capturedEvents) {
	t.Helper()
	plan := &domain.ExecutionPlan{Subject: "spend"}
	for i := 0; i < steps; i++ {
		st := domain.Step{Query: "step " + strconv.Itoa(i)}
		if i > 0 {
			st.DependsOn = []int{i - 1}
		}
		plan.Steps = append(plan.Steps, st)
	}
	s := minimalServer(t)
	s.Planner = &mockPlanner{responses: []plannerResponse{{plan: plan}}}
	s.SessionMgr = session.New(newMemSessionStore())
	s.LLMGateway = gw
	s.SpendMeter = meter
	d := &streamingDispatcher{gw: gw}
	s.Dispatcher = d
	ev := &capturedEvents{}
	s.TokenRecorder = ev
	return s, d, ev
}

// ADR-0131 gate criterion 2: a plan that exceeds execution.session.max_session_tokens
// fails with ErrSpendCeiling (ResourceExhausted on the wire) and leaves a record.
// Step 0 is admitted and books 150 provider-reported tokens against a 100-token
// ceiling; step 1 is never dispatched, and the plan does not come back as a
// success or a partial result.
func TestExecute_PlanOverSpendCeilingFailsWithErrSpendCeiling(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(100, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30})
	s, d, ev := ceilingServer(t, meter, gw, 3)

	resp, err := s.Execute(context.Background(), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if err == nil {
		t.Fatalf("plan over the ceiling returned success: %+v", resp.GetMetadata())
	}
	if !errors.Is(err, llm.ErrSpendCeiling) {
		t.Fatalf("want ErrSpendCeiling, got %v", err)
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted on the wire, got %v", status.Code(err))
	}
	if d.calls != 1 {
		t.Fatalf("only the first step may be dispatched, got %d dispatches", d.calls)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	var rec *domain.TaskEvent
	for i := range ev.events {
		if ev.events[i].AgentID == spendCeilingAgentID {
			rec = &ev.events[i]
		}
	}
	if rec == nil || !rec.BudgetOverrun || rec.TotalTokens != 150 || rec.PromptTokens != 120 {
		t.Fatalf("spend-ceiling record = %+v (events %+v)", rec, ev.events)
	}
}

// Under the ceiling the plan succeeds and the response reports what it spent,
// from provider usage (ADR-0131 gate criterion 1).
func TestExecute_ReportsPlanSpendFromProviderUsage(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30, cacheRead: 100})
	s, _, _ := ceilingServer(t, meter, gw, 2)

	resp, err := s.Execute(context.Background(), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	md := resp.GetMetadata()
	if md[MetaLLMInputTokens] != "240" || md[MetaLLMOutputTokens] != "60" || md[MetaLLMCacheReadTokens] != "200" ||
		md[MetaLLMCalls] != "2" || md[MetaLLMEstimatedCalls] != "0" {
		t.Fatalf("spend metadata = in %q out %q cache %q calls %q est %q", md[MetaLLMInputTokens],
			md[MetaLLMOutputTokens], md[MetaLLMCacheReadTokens], md[MetaLLMCalls], md[MetaLLMEstimatedCalls])
	}
}

// A refused stream never reaches the model, and the lease accounting is the
// SUM of every call made under it (an agent's ReAct loop makes several).
func TestGateway_LeaseUsageSumsCallsAndKeepsProviderUsage(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "abc", prompt: 10, compl: 5, cacheRead: 4})
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, 0)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "ses-g"})
	for i := 0; i < 2; i++ {
		out := make(chan domain.StreamChunk, 8)
		if err := gw.StreamChunks(context.Background(), lease, "p", domain.GenerateOptions{}, out); err != nil {
			t.Fatalf("StreamChunks: %v", err)
		}
	}
	u, err := gw.Complete(context.Background(), lease)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.TotalTokens != 30 || u.PromptTokens != 20 || u.CompletionTokens != 10 || u.CacheReadTokens != 8 || u.Estimated {
		t.Fatalf("lease usage = %+v, want 2 x (10 in / 5 out / 4 cached), not estimated", u)
	}
	if got := meter.Session("ses-g"); got.Calls != 2 || got.TotalTokens() != 30 {
		t.Fatalf("meter = %+v, want the same two calls under the lease's session", got)
	}
}

// ChargeLease adds the retrieval chain's usage to a live step lease and ignores a
// lease that is gone; the completion hook runs once per completed lease.
func TestGateway_ChargeLeaseAndCompleteHook(t *testing.T) {
	t.Parallel()
	gw := meteredGateway(nil, &usageStreamer{text: "x"})
	var completed []domain.LeaseID
	gw.SetCompleteHook(func(l domain.LeaseID) { completed = append(completed, l) })

	step, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, 0)
	gw.ChargeLease(step, llm.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, Estimated: true})
	gw.ChargeLease("gone", llm.TokenUsage{TotalTokens: 99})
	u, _ := gw.Complete(context.Background(), step)
	if u.TotalTokens != 10 || u.PromptTokens != 7 || !u.Estimated {
		t.Fatalf("charged usage = %+v", u)
	}
	if len(completed) != 1 || completed[0] != step {
		t.Fatalf("completion hook calls = %v", completed)
	}
	if gw.SessionCount() != 0 {
		t.Fatal("ChargeLease on an unknown lease must not create one")
	}
}
