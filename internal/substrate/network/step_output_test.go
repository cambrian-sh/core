package network

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/hooks"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/stepoutput"
	"github.com/cambrian-sh/core/internal/testing/fakemodels"
)

// ADR-0139 amendment 2026-11-06: the live step-output stream's gateway and agent
// producers, driven through the real Server and SubstrateLLMGateway.

// streamingModels is a StepModels that can also stream (ObservedStepModels, which
// *llm.Provider implements): it replays a fixed delta script, then answers with
// the fixed response — the "fake streaming generator".
type streamingModels struct {
	*fakemodels.Models
	script []domain.ModelStreamDelta
	resp   domain.GenerateResponse
}

func (m *streamingModels) GenerateOnObserved(ctx context.Context, candidates []string, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (domain.GenerateResponse, string, error) {
	for _, d := range m.script {
		observe(d)
	}
	return m.resp, "m1", nil
}

// timeline records step-output chunks and exchanges in the order they happened.
type timeline struct {
	mu     sync.Mutex
	events []string
	chunks []domain.StepOutputChunk
}

func (tl *timeline) sink(c domain.StepOutputChunk) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, "chunk:"+c.Data)
	tl.chunks = append(tl.chunks, c)
}

func (tl *timeline) exchange(string, string, string, int, string, string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, "llm_exchange")
}

func (tl *timeline) snapshot() ([]string, []domain.StepOutputChunk) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]string(nil), tl.events...), append([]domain.StepOutputChunk(nil), tl.chunks...)
}

var finalAnswerTurn = domain.GenerateResponse{
	Text: "Thinking.", StopReason: domain.StopToolUse, Model: "m-served",
	ToolCalls: []domain.ModelToolCall{{ID: "c1", Name: "final_answer", Arguments: []byte(`{"answer":"Hello é"}`)}},
}

var finalAnswerScript = []domain.ModelStreamDelta{
	{Text: "Thinking."},
	{ToolCall: &domain.ToolCallDelta{Index: 1, ID: "c1", Name: "final_answer"}},
	{ToolCall: &domain.ToolCallDelta{Index: 1, ArgumentsDelta: `{"answer":"Hel`}},
	{ToolCall: &domain.ToolCallDelta{Index: 1, ArgumentsDelta: `lo \u00`}},
	{ToolCall: &domain.ToolCallDelta{Index: 1, ArgumentsDelta: `e9"}`}},
	// Another tool's arguments are never forwarded.
	{ToolCall: &domain.ToolCallDelta{Index: 2, ID: "c2", Name: "write_file", ArgumentsDelta: `{"answer":"secret"}`}},
}

// stepOutputServer is c1Server with a streaming model source and a step-output
// stream recording into tl. opts may set hooks / streams_output.
func stepOutputServer(t *testing.T, models StepModels, mod func(*stepoutput.Options)) (*Server, domain.LeaseID, context.Context, *timeline) {
	t.Helper()
	s, gw := newLeaseServer(t)
	gw.SetStepModels(models)
	lease, err := gw.Acquire(context.Background(),
		domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", StepIndex: 4, AgentID: "a1"})
	tl := &timeline{}
	o := stepoutput.Options{Leases: gw, Sinks: []stepoutput.Sink{tl.sink}}
	if mod != nil {
		mod(&o)
	}
	s.StepOutput = stepoutput.New(o)
	s.LLMExchangeSink = tl.exchange
	return s, lease, ctxWithMD("x-agent-id", "a1"), tl
}

func nativeTurn(t *testing.T, s *Server, ctx context.Context, lease domain.LeaseID) *pb.GenerateWithToolsResponse {
	t.Helper()
	resp, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId:  string(lease),
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "say hello"}},
		Tools:    []*pb.ToolDefinitionProto{{Name: "final_answer", ParametersJson: `{}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGenerateWithTools_NativeTurnStreamsStepOutputBeforeItsExchange(t *testing.T) {
	t.Parallel()
	models := &streamingModels{Models: fakemodels.New().Set("m1", c1Model{}), script: finalAnswerScript, resp: finalAnswerTurn}
	s, lease, ctx, tl := stepOutputServer(t, models, nil)

	resp := nativeTurn(t, s, ctx, lease)
	// The RPC's answer is unchanged: unary, the whole turn.
	if len(resp.GetToolCalls()) != 1 || resp.GetToolCalls()[0].GetArgumentsJson() != `{"answer":"Hello é"}` {
		t.Fatalf("response = %+v", resp)
	}
	events, chunks := tl.snapshot()
	want := []string{"chunk:Thinking.", "chunk:Hel", "chunk:lo ", "chunk:é", "llm_exchange"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("timeline = %q\nwant       %q", events, want)
	}
	for i, c := range chunks {
		if c.LeaseID != lease || c.SessionID != "s1" || c.StepIndex != 4 || c.AgentID != "a1" ||
			c.Source != domain.StepOutputFromGateway || c.Kind != domain.StepOutputText || c.Seq != uint64(i+1) {
			t.Errorf("chunk %d = %+v", i, c)
		}
	}
}

func TestGenerateWithTools_FinalAnswerToolIsConfigured(t *testing.T) {
	t.Parallel()
	script := []domain.ModelStreamDelta{
		{ToolCall: &domain.ToolCallDelta{Index: 0, Name: "respond", ArgumentsDelta: `{"body":"configured"}`}},
		{ToolCall: &domain.ToolCallDelta{Index: 1, Name: "final_answer", ArgumentsDelta: `{"answer":"default name"}`}},
	}
	models := &streamingModels{Models: fakemodels.New(), script: script, resp: finalAnswerTurn}
	s, lease, ctx, tl := stepOutputServer(t, models, func(o *stepoutput.Options) {
		o.Config = config.StepOutputConfig{FinalAnswerTool: "respond", FinalAnswerArg: "body"}
	})
	nativeTurn(t, s, ctx, lease)
	_, chunks := tl.snapshot()
	if len(chunks) != 1 || chunks[0].Data != "configured" {
		t.Fatalf("chunks = %+v; only the configured tool/arg is the deliverable", chunks)
	}
}

func TestGenerateWithTools_ProviderWithoutStreamingFallsBackUnaryWithNoDeltas(t *testing.T) {
	t.Parallel()
	// A plain fakemodels source has no GenerateOnObserved: the turn is served unary.
	fm := fakemodels.New().Set("m1", c1Model{resp: finalAnswerTurn})
	s, lease, ctx, tl := stepOutputServer(t, fm, nil)
	resp := nativeTurn(t, s, ctx, lease)
	if len(resp.GetToolCalls()) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	events, _ := tl.snapshot()
	if strings.Join(events, "|") != "llm_exchange" {
		t.Fatalf("timeline = %q; want no deltas, then the exchange", events)
	}
}

func TestGenerateWithTools_StreamsOutputAgentGetsNoGatewayDeltas(t *testing.T) {
	t.Parallel()
	// Suppressed ⇒ served unary (GenerateOn), so the source must also answer that way.
	models := &streamingModels{Models: fakemodels.New().Set("m1", c1Model{resp: finalAnswerTurn}),
		script: finalAnswerScript, resp: finalAnswerTurn}
	s, lease, ctx, tl := stepOutputServer(t, models, func(o *stepoutput.Options) {
		o.StreamsOutput = func(_ context.Context, agent string) bool { return agent == "a1" }
	})
	nativeTurn(t, s, ctx, lease)
	if _, chunks := tl.snapshot(); len(chunks) != 0 {
		t.Fatalf("a streams_output agent's step streamed twice: %+v", chunks)
	}
}

// redactHook is a redact_output-like post_model hook.
type redactHook struct{}

func (redactHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "redact", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true}
}
func (redactHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	t := strings.ReplaceAll(ev.Text, "Thinking", "[redacted]")
	return domain.HookResult{Text: &t}, nil
}

func TestGenerateWithTools_RedactionHookSuppressesDeltas(t *testing.T) {
	t.Parallel()
	chain, err := domain.NewHookChain([]domain.Hook{redactHook{}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	models := &streamingModels{Models: fakemodels.New().Set("m1", c1Model{resp: finalAnswerTurn}),
		script: finalAnswerScript, resp: finalAnswerTurn}
	s, lease, ctx, tl := stepOutputServer(t, models, func(o *stepoutput.Options) { o.Hooks = chain })
	nativeTurn(t, s, ctx, lease)
	events, chunks := tl.snapshot()
	if len(chunks) != 0 {
		t.Fatalf("a post_model hook is installed: no pre-hook text may stream, got %+v", chunks)
	}
	if strings.Join(events, "|") != "llm_exchange" {
		t.Fatalf("timeline = %q", events)
	}
}

// scopedRedactHook is redactHook with a static scope.
type scopedRedactHook struct{ agents, purposes []string }

func (h scopedRedactHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "scoped-redact", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true,
		Agents: h.agents, Purposes: h.purposes}
}
func (scopedRedactHook) Run(ctx context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	return redactHook{}.Run(ctx, ev)
}

// T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or: a post_model hook
// scoped away from this step (the planner, another agent) no longer suppresses its
// live deltas; one scoped to this step's agent does.
func TestGenerateWithTools_SuppressionHonoursHookScope(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		hook    scopedRedactHook
		streams bool
	}{
		{"planner only", scopedRedactHook{purposes: []string{"planner"}}, true},
		{"another agent", scopedRedactHook{agents: []string{"other"}}, true},
		{"this agent's steps", scopedRedactHook{agents: []string{"a*"}, purposes: []string{"agent_step"}}, false},
	} {
		chain, err := domain.NewHookChain([]domain.Hook{c.hook}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		models := &streamingModels{Models: fakemodels.New().Set("m1", c1Model{resp: finalAnswerTurn}),
			script: finalAnswerScript, resp: finalAnswerTurn}
		s, lease, ctx, tl := stepOutputServer(t, models, func(o *stepoutput.Options) { o.Hooks = chain })
		nativeTurn(t, s, ctx, lease)
		if _, chunks := tl.snapshot(); (len(chunks) > 0) != c.streams {
			t.Errorf("%s: streamed %d chunks, want streaming=%v", c.name, len(chunks), c.streams)
		}
	}
}

func TestGenerateViaModelStream_TextLaneFeedsTheSameStream(t *testing.T) {
	t.Parallel()
	fm := fakemodels.New().Set("m1", c1Model{resp: domain.GenerateResponse{Text: "streamed text", StopReason: domain.StopEndTurn}})
	s, lease, ctx, tl := stepOutputServer(t, fm, nil)
	rec := &chunkRecorder{ctx: ctx}
	if err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(lease),
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "go"}}}, rec); err != nil {
		t.Fatal(err)
	}
	_, chunks := tl.snapshot()
	if len(chunks) != 1 || chunks[0].Data != "streamed text" || chunks[0].LeaseID != lease || chunks[0].StepIndex != 4 {
		t.Fatalf("chunks = %+v", chunks)
	}
}

// ── EmitStepOutput (the agent producer) ──────────────────────────────────────

// pushStream is the server side of a client-streaming EmitStepOutput call.
type pushStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*pb.StepOutputChunk
	ack    *pb.EmitStepOutputAck
}

func (p *pushStream) Context() context.Context { return p.ctx }
func (p *pushStream) Recv() (*pb.StepOutputChunk, error) {
	if len(p.chunks) == 0 {
		return nil, io.EOF
	}
	c := p.chunks[0]
	p.chunks = p.chunks[1:]
	return c, nil
}
func (p *pushStream) SendAndClose(a *pb.EmitStepOutputAck) error {
	p.ack = a
	return nil
}

func TestEmitStepOutput_LeaseBoundSeqDedupeAndFinal(t *testing.T) {
	t.Parallel()
	s, lease, ctx, tl := stepOutputServer(t, fakemodels.New(), nil)
	ps := &pushStream{ctx: ctx, chunks: []*pb.StepOutputChunk{
		{LeaseId: string(lease), Seq: 1, Data: "a"},
		{LeaseId: string(lease), Seq: 2, Kind: pb.StepOutputKind_STEP_OUTPUT_KIND_PROGRESS, Data: "half"},
		{LeaseId: string(lease), Seq: 2, Data: "dup"},
		{LeaseId: string(lease), Seq: 3, Data: "b", Final: true},
		{LeaseId: string(lease), Seq: 4, Data: "late"},
	}}
	if err := s.EmitStepOutput(ps); err != nil {
		t.Fatal(err)
	}
	if a := ps.ack; a.GetAccepted() != 3 || a.GetDuplicates() != 1 || a.GetAfterFinal() != 1 || a.GetLastSeq() != 3 {
		t.Fatalf("ack = %+v", a)
	}
	_, chunks := tl.snapshot()
	if len(chunks) != 3 || chunks[1].Kind != domain.StepOutputProgress || !chunks[2].Final ||
		chunks[0].Source != domain.StepOutputFromAgent || chunks[0].SessionID != "s1" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestEmitStepOutput_DeadForeignAndMissingLeasesAreRefused(t *testing.T) {
	t.Parallel()
	s, lease, ctx, tl := stepOutputServer(t, fakemodels.New(), nil)
	cases := []struct {
		name string
		ctx  context.Context
		c    *pb.StepOutputChunk
		want codes.Code
	}{
		{"dead lease", ctx, &pb.StepOutputChunk{LeaseId: "lease-that-never-was", Seq: 1, Data: "x"}, codes.PermissionDenied},
		{"foreign lease", ctxWithMD("x-agent-id", "intruder"), &pb.StepOutputChunk{LeaseId: string(lease), Seq: 1, Data: "x"}, codes.PermissionDenied},
		{"no lease", ctx, &pb.StepOutputChunk{Seq: 1, Data: "x"}, codes.InvalidArgument},
		{"seq zero", ctx, &pb.StepOutputChunk{LeaseId: string(lease), Data: "x"}, codes.InvalidArgument},
	}
	for _, c := range cases {
		err := s.EmitStepOutput(&pushStream{ctx: c.ctx, chunks: []*pb.StepOutputChunk{c.c}})
		if status.Code(err) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, chunks := tl.snapshot(); len(chunks) != 0 {
		t.Fatalf("a refused push was delivered: %+v", chunks)
	}
}

func TestEmitStepOutput_UnwiredIsUnimplemented(t *testing.T) {
	t.Parallel()
	s, _ := newLeaseServer(t)
	if err := s.EmitStepOutput(&pushStream{ctx: context.Background()}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v", err)
	}
}

func TestLeaseCompletionEndsStepOutputState(t *testing.T) {
	t.Parallel()
	// NewServer installs the gateway's lease-complete hook; that hook must end the
	// stream's per-lease state.
	_, gw := newLeaseServer(t)
	srv := NewServer(nil, nil, nil, config.ExecutionConfig{}, nil, nil, nil, nil, nil, nil, gw, nil, nil)
	srv.StepOutput = stepoutput.New(stepoutput.Options{Leases: gw})
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	srv.StepOutput.EmitGateway(lease, domain.StepOutputText, "x")
	if srv.StepOutput.Tracked() != 1 {
		t.Fatal("no state tracked")
	}
	if _, err := gw.Complete(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if n := srv.StepOutput.Tracked(); n != 0 {
		t.Fatalf("lease completion left %d entries", n)
	}
}

// T-20261106-post-model-hooks-never-see-native-tool-call, end to end: an agent's
// final_answer that quotes a secret comes back to the SDK (the GenerateWithTools
// RPC) redacted by an operator's redact_output hook at post_model — through the
// real gateway and the real llm.Provider over a streaming model endpoint — and no
// pre-redaction byte reaches the live step-output stream.
func TestGenerateWithTools_FinalAnswerSecretIsRedactedEndToEnd(t *testing.T) {
	t.Parallel()
	const secret = "sk-e2e-AAAAAAAAAAAAAAAAAAAA"
	sse := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Thinking."},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"final_answer","arguments":"{\"answer\":\"the key is sk-e2e-"}}]},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"AAAAAAAAAAAAAAAAAAAA\",\"type\":\"text\"}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	// The same turn, unary: with a post_model hook installed the gateway does not
	// observe, so the call is served unary (either way the hook sees the whole reply).
	unary := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"Thinking.","tool_calls":[{"id":"c1","type":"function","function":{"name":"final_answer","arguments":"{\"answer\":\"the key is ` + secret + `\",\"type\":\"text\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, unary)
	}))
	t.Cleanup(srv.Close)

	hs, err := hooks.FromConfig(config.HooksConfig{Entries: []config.HookConfig{{
		Name: "redact-keys", Kind: config.HookKindRedactOutput, Pattern: `sk-[A-Za-z0-9-]+`,
		MayRewrite: true, Points: []string{"post_model"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := llm.NewProvider(config.LLMProviderConfig{
		Default: "m1",
		Generators: []config.GeneratorConfig{{ID: "m1", Provider: "openai", Model: "m", Endpoint: srv.URL,
			TimeoutMs: 5000, NativeTools: true}},
		Health: config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, llm.WithHooks(chain))
	if err != nil {
		t.Fatal(err)
	}
	s, lease, ctx, tl := stepOutputServer(t, provider, func(o *stepoutput.Options) { o.Hooks = chain })

	resp := nativeTurn(t, s, ctx, lease)
	if len(resp.GetToolCalls()) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	args := resp.GetToolCalls()[0].GetArgumentsJson()
	answer, err := domain.ToolCallStringArg([]byte(args), "answer")
	if err != nil || answer != "the key is [REDACTED]" || strings.Contains(args, secret) {
		t.Fatalf("the SDK would read answer %q (args %s, err %v)", answer, args, err)
	}
	if kind, _ := domain.ToolCallStringArg([]byte(args), "type"); kind != "text" {
		t.Errorf("the final call's other fields must survive: %s", args)
	}
	if _, chunks := tl.snapshot(); len(chunks) != 0 {
		t.Fatalf("a post_model hook is installed: no live delta may stream, got %+v", chunks)
	}
}

// T-20261106-text-protocol-agent-replies-never-reach-post: the TEXT lane
// (GenerateViaModelStream, the SDK's text protocol) under a matching post_model
// redaction hook, through the real gateway and the real llm.Provider. The SDK
// receives only redacted text — the final answer written as text included — the
// final chunk keeps its C1 metadata, and no pre-hook byte reaches the live
// step-output stream. A deny ends the RPC with the native lane's status and no chunk.
func TestGenerateViaModelStream_TextLaneReplyCrossesPostModel(t *testing.T) {
	t.Parallel()
	const secret = "sk-e2e-BBBBBBBBBBBBBBBBBBBB"
	sse := `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[{"index":0,"delta":{"content":"{\"action\":\"final_answer\",\"answer\":\"the key is sk-e2e-"},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[{"index":0,"delta":{"content":"BBBBBBBBBBBBBBBBBBBB\"}"},"finish_reason":"stop"}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)

	run := func(t *testing.T, hs []domain.Hook) (*chunkRecorder, error, []domain.StepOutputChunk) {
		chain, err := domain.NewHookChain(hs, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		provider, err := llm.NewProvider(config.LLMProviderConfig{
			Default:    "m1",
			Generators: []config.GeneratorConfig{{ID: "m1", Provider: "openai", Model: "m", Endpoint: srv.URL, TimeoutMs: 5000}},
			Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
		}, nil, llm.WithHooks(chain))
		if err != nil {
			t.Fatal(err)
		}
		s, lease, ctx, tl := stepOutputServer(t, provider, func(o *stepoutput.Options) { o.Hooks = chain })
		// The agent-plane interceptor's principal (agent_auth.go), as in production.
		ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal("a1"))
		rec := &chunkRecorder{ctx: ctx}
		err = s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(lease),
			Messages: []*pb.ModelMessageProto{{Role: "user", Content: "go"}}}, rec)
		_, chunks := tl.snapshot()
		return rec, err, chunks
	}

	t.Run("redact", func(t *testing.T) {
		hs, err := hooks.FromConfig(config.HooksConfig{Entries: []config.HookConfig{{Name: "redact-keys",
			Kind: config.HookKindRedactOutput, Pattern: `sk-[A-Za-z0-9-]+`, MayRewrite: true,
			Points: []string{"post_model"}, Agents: []string{"a*"}, Purposes: []string{"agent_step"}}}})
		if err != nil {
			t.Fatal(err)
		}
		rec, err, live := run(t, hs)
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, c := range rec.chunks {
			text.WriteString(c.GetText())
		}
		if got := text.String(); got != `{"action":"final_answer","answer":"the key is [REDACTED]"}` || strings.Contains(got, "sk-e2e") {
			t.Fatalf("the SDK would read %q", got)
		}
		last := rec.chunks[len(rec.chunks)-1]
		if !last.GetIsFinal() || last.GetUsageTokens() != 12 || last.GetStopReason() != string(domain.StopEndTurn) ||
			last.GetModel() != "m-served" {
			t.Fatalf("final chunk lost its C1 metadata: %+v", last)
		}
		for _, c := range live {
			if strings.Contains(c.Data, "sk-e2e") {
				t.Fatalf("pre-hook text reached the live step-output stream: %+v", live)
			}
		}
		if len(live) != 0 {
			t.Fatalf("a matching post_model hook suppresses gateway deltas, got %+v", live)
		}
	})

	t.Run("deny", func(t *testing.T) {
		rec, err, live := run(t, []domain.Hook{denyTextHook{needle: "sk-e2e"}})
		if err == nil || status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "hook") {
			t.Fatalf("err = %v; want the native lane's hook-refusal status", err)
		}
		if len(rec.chunks) != 0 || len(live) != 0 {
			t.Fatalf("a refused reply forwarded chunks: sdk %+v live %+v", rec.chunks, live)
		}
	})
}

// denyTextHook refuses a model reply whose text contains needle (a post_model
// deny; no built-in config kind denies at post_model, a command hook would).
type denyTextHook struct{ needle string }

func (denyTextHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "deny-text", Points: []domain.HookPoint{domain.HookPostModel}}
}
func (h denyTextHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	if strings.Contains(ev.Text, h.needle) {
		return domain.HookResult{Verdict: domain.HookDeny, Reason: "secret in reply"}, nil
	}
	return domain.HookResult{}, nil
}
