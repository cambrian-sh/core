package network

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/stepoutput"
)

// T-20261112-pre-send-context-window-check-per-model-with (owner decision
// 2026-11-18): the gateway checks a prompt against the model's configured
// context window before sending it. An over-window prompt is answered OutOfRange
// (deterministic) without the provider ever seeing it; a near-window prompt is
// sent. End to end: the real gateway over the real llm.Provider and a real
// OpenAI-compatible client against a fake provider that counts its requests.

// windowGatewayServer serves a lease on m1 (context window `window`, output cap
// 100, default 5% margin), falling back to an equivalent m2 with no window.
func windowGatewayServer(t *testing.T, m1Endpoint, m2Endpoint string, window int) (*Server, domain.LeaseID, context.Context) {
	t.Helper()
	cfg := config.LLMProviderConfig{
		Default: "m1",
		Generators: []config.GeneratorConfig{
			{ID: "m1", Provider: "openai", Model: "m", Endpoint: m1Endpoint, TimeoutMs: 5000, NativeTools: true, MaxTokens: 100, ContextWindowTokens: window},
			{ID: "m2", Provider: "openai", Model: "m", Endpoint: m2Endpoint, TimeoutMs: 5000, NativeTools: true},
		},
		EquivalentGenerators: [][]string{{"m1", "m2"}},
		Health:               config.HealthConfig{FailureThreshold: 1, CooldownMs: 60000},
	}
	provider, err := llm.NewProvider(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, gw := newLeaseServer(t)
	gw.SetStepModels(provider)
	alloc := domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}
	alloc.Fallbacks[0] = domain.AgentDefinition{ID: "llm:m2"}
	lease, err := gw.Acquire(context.Background(), alloc, 1_000_000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", StepIndex: 0, AgentID: "a1"})
	tl := &timeline{}
	s.StepOutput = stepoutput.New(stepoutput.Options{Leases: gw, Sinks: []stepoutput.Sink{tl.sink}})
	s.LLMExchangeSink = tl.exchange
	return s, lease, ctxWithMD("x-agent-id", "a1")
}

func turnWith(s *Server, ctx context.Context, lease domain.LeaseID, content string) (*pb.GenerateWithToolsResponse, error) {
	return s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId:  string(lease),
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: content}},
		Tools:    []*pb.ToolDefinitionProto{{Name: "final_answer", ParametersJson: `{}`}},
	})
}

func TestGatewayE2E_OverWindowPromptNeverReachesTheProvider(t *testing.T) {
	t.Parallel()
	m1, m1Hits := faultServer(t, "")
	m2, m2Hits := faultServer(t, "")
	// Window 2000: budget = 2000 - 100 output - 100 margin = 1800 prompt tokens.
	s, lease, ctx := windowGatewayServer(t, m1.URL, m2.URL, 2000)

	// 3000 base64 characters: ~2308 tokens content-aware. len/4 would have
	// estimated 750 and sent it.
	over := "attachment: " + strings.Repeat("QUJDZGVmMTIz", 250)
	_, err := turnWith(s, ctx, lease, over)
	if got := status.Code(err); got != codes.OutOfRange {
		t.Fatalf("over-window: code = %v (%v), want OutOfRange", got, err)
	}
	if m1Hits.Load() != 0 {
		t.Fatalf("the over-window prompt reached the provider (%d requests)", m1Hits.Load())
	}
	// A request fault never falls back: the equivalent model is not asked either.
	if m2Hits.Load() != 0 {
		t.Fatalf("the over-window prompt fell back to the equivalent model (%d requests)", m2Hits.Load())
	}

	// Near the window (~1700 prompt tokens, under the 1800 budget): sent, served
	// by m1 — the refusal above did not open its breaker (threshold 1).
	near := strings.Repeat("word ", 1360)
	resp, err := turnWith(s, ctx, lease, near)
	if err != nil || !strings.Contains(resp.GetText(), "hello") {
		t.Fatalf("near-window turn = %+v, %v; want m1's reply", resp, err)
	}
	if m1Hits.Load() != 1 || m2Hits.Load() != 0 {
		t.Fatalf("m1 hits %d, m2 hits %d; want the near-window prompt sent to m1 once", m1Hits.Load(), m2Hits.Load())
	}
}
