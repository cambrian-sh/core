package network

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/fakemodels"
)

// ADR-0139 wave 1c: both agent-plane RPCs build ONE C1 GenerateRequest and the
// gateway hands it, whole, to the Provider stack. These tests drive the real
// Server and the real SubstrateLLMGateway; only the model source is a fake, and it
// records what "the adapter" received.

// c1Model is a RequestGenerator fake with a fixed reply.
type c1Model struct{ resp domain.GenerateResponse }

func (m c1Model) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return m.resp, nil
}

func (m c1Model) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	r := m.resp
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: r.Text}
	ch <- domain.StreamChunk{IsFinal: true, UsagePromptTokens: r.Usage.Input, UsageCompletionTokens: r.Usage.Output,
		UsageTotalTokens: r.Usage.Input + r.Usage.Output, Response: &r}
	close(ch)
	return ch, nil
}

func (m c1Model) Capabilities() domain.Capabilities { return domain.Capabilities{} }

var c1Reply = domain.GenerateResponse{
	Text:       `{"ok":true}`,
	StopReason: domain.StopEndTurn,
	Usage:      domain.GenerateUsage{Input: 40, Output: 6, CacheRead: 30, CacheWrite: 8, CacheWriteKnown: true},
	Ignored:    []string{domain.FieldTemperature},
	Model:      "claude-test-1",
}

// c1Server is a server whose gateway serves "llm:m1" from model, with a lease bound
// to agent a1 (the caller every test presents through x-agent-id).
func c1Server(t *testing.T, model any) (*Server, *fakemodels.Models, domain.LeaseID, context.Context) {
	t.Helper()
	s, gw := newLeaseServer(t)
	fm := useFakeModel(gw, "llm:m1", model)
	lease, err := gw.Acquire(context.Background(),
		domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", StepIndex: 0, AgentID: "a1"})
	return s, fm, lease, ctxWithMD("x-agent-id", "a1")
}

// chunkRecorder is the server side of GenerateViaModelStream, keeping every chunk.
type chunkRecorder struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*pb.GenerateChunk
}

func (r *chunkRecorder) Context() context.Context { return r.ctx }
func (r *chunkRecorder) Send(c *pb.GenerateChunk) error {
	r.chunks = append(r.chunks, c)
	return nil
}

func ptr[T any](v T) *T { return &v }

var c1RequestFields = struct {
	system   []*pb.SystemBlock
	messages []*pb.ModelMessageProto
	schema   *pb.ResponseSchema
	options  *pb.GenerateOptions
}{
	system: []*pb.SystemBlock{{Text: "you are stable", Cache: true}, {Text: "tier two"}},
	messages: []*pb.ModelMessageProto{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []*pb.ModelToolCallProto{{Id: "c1", Name: "t", ArgumentsJson: `{"a":1}`}}},
		{Role: "tool", ToolCallId: "c1", Content: "done"},
	},
	schema: &pb.ResponseSchema{Name: "verdict", SchemaJson: `{"type":"object"}`, Strict: true},
	// Temperature 0 is SET: field presence is what lets it reach the adapter.
	options: &pb.GenerateOptions{MaxTokens: ptr(int32(256)), Temperature: ptr(float32(0)),
		StopSequences: []string{"END"}, Strict: true},
}

// assertC1Fields checks the request the adapter received carries every field.
func assertC1Fields(t *testing.T, got domain.GenerateRequest) {
	t.Helper()
	if len(got.System) != 2 || got.System[0].Text != "you are stable" || !got.System[0].Cache || got.System[1].Cache {
		t.Errorf("system blocks = %+v", got.System)
	}
	if len(got.Messages) != 3 || got.Messages[1].ToolCalls[0].ID != "c1" || got.Messages[2].ToolCallID != "c1" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if rs := got.ResponseSchema; rs == nil || rs.Name != "verdict" || string(rs.Schema) != `{"type":"object"}` || !rs.Strict {
		t.Errorf("response schema = %+v", got.ResponseSchema)
	}
	o := got.Options
	if o.MaxTokens != 256 || o.Temperature == nil || *o.Temperature != 0 || len(o.Stop) != 1 || o.Stop[0] != "END" || !o.Strict {
		t.Errorf("options = %+v (temperature %v)", o, o.Temperature)
	}
	if got.CacheTTL != domain.CacheTTLLong {
		t.Errorf("cache ttl = %q, want long", got.CacheTTL)
	}
}

func TestGenerateViaModelStream_C1FieldsReachTheAdapter_AndFinalChunkCarriesTheResponse(t *testing.T) {
	t.Parallel()
	s, fm, lease, ctx := c1Server(t, c1Model{resp: c1Reply})
	f := c1RequestFields
	rec := &chunkRecorder{ctx: ctx}
	err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{
		LeaseId: string(lease), System: f.system, Messages: f.messages,
		ResponseSchema: f.schema, Options: f.options, CacheTtl: pb.CacheTTL_CACHE_TTL_LONG,
	}, rec)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	reqs := fm.Requests()
	if len(reqs) != 1 {
		t.Fatalf("adapter saw %d requests", len(reqs))
	}
	assertC1Fields(t, reqs[0])

	last := rec.chunks[len(rec.chunks)-1]
	if !last.IsFinal {
		t.Fatalf("last chunk not final: %+v", last)
	}
	if len(last.Ignored) != 1 || last.Ignored[0] != domain.FieldTemperature {
		t.Errorf("ignored = %v, want [temperature]", last.Ignored)
	}
	u := last.GetUsage()
	if u.GetInputTokens() != 40 || u.GetOutputTokens() != 6 || u.GetCacheReadTokens() != 30 ||
		u.GetCacheWriteTokens() != 8 || !u.GetCacheWriteKnown() {
		t.Errorf("usage = %+v", u)
	}
	if last.StopReason != string(domain.StopEndTurn) || last.Model != "claude-test-1" {
		t.Errorf("stop_reason = %q model = %q", last.StopReason, last.Model)
	}
}

// An older SDK sends only the deprecated prompt and the old options: it is served
// exactly as before, as one user turn with nothing else set.
func TestGenerateViaModelStream_LegacyPromptOnlyStillWorks(t *testing.T) {
	t.Parallel()
	s, fm, lease, ctx := c1Server(t, textModel{})
	rec := &chunkRecorder{ctx: ctx}
	if err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{
		LeaseId: string(lease), Prompt: "legacy prompt",
	}, rec); err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text strings.Builder
	for _, c := range rec.chunks {
		text.WriteString(c.GetText())
	}
	if text.String() != "generated" {
		t.Fatalf("text = %q", text.String())
	}
	got := fm.Requests()[0]
	if len(got.Messages) != 1 || got.Messages[0].Role != domain.RoleUser || got.Messages[0].Content != "legacy prompt" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if len(got.System) != 0 || got.ResponseSchema != nil || got.Options.Temperature != nil || got.Options.MaxTokens != 0 {
		t.Errorf("a prompt-only request must carry nothing else: %+v", got)
	}
}

func TestGenerateWithTools_C1FieldsReachTheAdapter_AndIgnoredIsReturned(t *testing.T) {
	t.Parallel()
	s, fm, lease, ctx := c1Server(t, c1Model{resp: c1Reply})
	f := c1RequestFields
	resp, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId: string(lease), System: f.system, Messages: f.messages,
		Tools:          []*pb.ToolDefinitionProto{{Name: "t", ParametersJson: `{"type":"object"}`}},
		ResponseSchema: f.schema, Options: f.options, CacheTtl: pb.CacheTTL_CACHE_TTL_LONG,
	})
	if err != nil {
		t.Fatalf("GenerateWithTools: %v", err)
	}
	got := fm.Requests()[0]
	assertC1Fields(t, got)
	if len(got.Tools) != 1 || got.Tools[0].Name != "t" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if len(resp.Ignored) != 1 || resp.Ignored[0] != domain.FieldTemperature {
		t.Errorf("ignored = %v", resp.Ignored)
	}
	if resp.GetUsage().GetCacheReadTokens() != 30 || resp.UsageTokens != 46 || resp.Model != "claude-test-1" {
		t.Errorf("usage = %+v usage_tokens = %d model = %q", resp.GetUsage(), resp.UsageTokens, resp.Model)
	}
}

// A system prompt has one way in: the system field.
func TestManagedRPCs_RefuseASystemRoleMessage(t *testing.T) {
	t.Parallel()
	s, _, lease, ctx := c1Server(t, c1Model{resp: c1Reply})
	msgs := []*pb.ModelMessageProto{{Role: "system", Content: "be nice"}, {Role: "user", Content: "hi"}}
	if _, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{LeaseId: string(lease), Messages: msgs}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GenerateWithTools: want InvalidArgument, got %v", err)
	}
	err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(lease), Messages: msgs}, &chunkRecorder{ctx: ctx})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("GenerateViaModelStream: want InvalidArgument, got %v", err)
	}
}

// A model without native tools answers FailedPrecondition through the REAL gateway
// (the C1 tools refusal), the documented text-protocol fallback signal.
func TestGenerateWithTools_NoNativeToolsIsFailedPrecondition_ThroughTheGateway(t *testing.T) {
	t.Parallel()
	s, _, lease, ctx := c1Server(t, textModel{})
	_, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId:  string(lease),
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "hi"}},
		Tools:    []*pb.ToolDefinitionProto{{Name: "t", ParametersJson: `{}`}},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

// ADR-0079: a native tool turn reaches the exchange lane, with the conversation and
// the model's tool calls rendered (T-20261104-llm-exchange-capture-adr-0079-misses-every).
func TestGenerateWithTools_EmitsTheLLMExchange(t *testing.T) {
	t.Parallel()
	reply := domain.GenerateResponse{
		Text: "calling it", StopReason: domain.StopToolUse, Model: "m-served",
		ToolCalls: []domain.ModelToolCall{{ID: "c9", Name: "write_file", Arguments: []byte(`{"p":"x"}`)}},
	}
	s, _, lease, ctx := c1Server(t, c1Model{resp: reply})
	var mu sync.Mutex
	type exchange struct{ lease, agent, model, prompt, completion string }
	var got []exchange
	s.LLMExchangeSink = func(sessionID, agentID, modelID string, _ int, prompt, completion string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, exchange{sessionID, agentID, modelID, prompt, completion})
	}
	if _, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId:  string(lease),
		System:   []*pb.SystemBlock{{Text: "sys"}},
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "write x"}},
		Tools:    []*pb.ToolDefinitionProto{{Name: "write_file", ParametersJson: `{}`}},
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("exchanges = %d, want 1", len(got))
	}
	e := got[0]
	if e.lease != string(lease) || e.agent != "a1" || e.model != "m-served" {
		t.Errorf("exchange attribution = %+v", e)
	}
	for _, want := range []string{"[system]\nsys", "[user]\nwrite x", "[tools offered] write_file"} {
		if !strings.Contains(e.prompt, want) {
			t.Errorf("prompt %q lacks %q", e.prompt, want)
		}
	}
	if !strings.Contains(e.completion, "calling it") || !strings.Contains(e.completion, `→ tool_call c9 write_file({"p":"x"})`) {
		t.Errorf("completion = %q", e.completion)
	}
}

// The ADR-0139 D8 ownership check runs before the request is even built: a caller
// presenting a lease bound to another agent is refused on both lanes, with the new
// fields set (they must not open a path around it).
func TestManagedRPCs_LeaseOwnershipUnchangedWithC1Fields(t *testing.T) {
	t.Parallel()
	s, fm, lease, _ := c1Server(t, c1Model{resp: c1Reply})
	forger := ctxWithMD("x-agent-id", "someone-else")
	sys := []*pb.SystemBlock{{Text: "s"}}
	msgs := []*pb.ModelMessageProto{{Role: "user", Content: "hi"}}
	if _, err := s.GenerateWithTools(forger, &pb.GenerateWithToolsRequest{LeaseId: string(lease), System: sys, Messages: msgs}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("GenerateWithTools: want PermissionDenied, got %v", err)
	}
	if err := s.GenerateViaModelStream(&pb.GenerateStreamRequest{LeaseId: string(lease), System: sys, Messages: msgs},
		&chunkRecorder{ctx: forger}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("GenerateViaModelStream: want PermissionDenied, got %v", err)
	}
	if n := len(fm.Requests()); n != 0 {
		t.Errorf("a refused caller reached the model %d times", n)
	}
}
