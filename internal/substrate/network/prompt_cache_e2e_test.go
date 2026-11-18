package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ENG-126 (owner decision 2026-11-18: verify the ADR-0139 prompt caching on the
// default lane and make the saving visible). Two consecutive agent turns go the
// production path an SDK agent's native turn takes: the agent-plane
// GenerateWithTools RPC -> the gateway (lease, meter) -> the real llm.Provider ->
// the real Anthropic adapter -> a fake Messages API that records the bytes it
// receives and answers like Anthropic does once a prefix is cached (the first call
// writes it, the next one reads it).

// cachingMessagesAPI is a fake Anthropic Messages endpoint. Call n>1 reports the
// first call's cache write as a cache read, as the real API does for an unchanged
// prefix.
type cachingMessagesAPI struct {
	mu     sync.Mutex
	bodies [][]byte
}

const cachedPrefixTokens = 1800

func (a *cachingMessagesAPI) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.bodies = append(a.bodies, raw)
	n := len(a.bodies)
	a.mu.Unlock()
	read, write := 0, cachedPrefixTokens
	if n > 1 {
		read, write = cachedPrefixTokens, 0
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_%d","type":"message","role":"assistant","model":"claude-served",
 "content":[{"type":"tool_use","id":"toolu_%d","name":"final_answer","input":{"answer":"ok"}}],
 "stop_reason":"tool_use","stop_sequence":null,
 "usage":{"input_tokens":40,"output_tokens":12,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, n, n, read, write)
}

func (a *cachingMessagesAPI) body(t *testing.T, i int) map[string]json.RawMessage {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a.bodies[i], &m); err != nil {
		t.Fatalf("request %d is not JSON: %v", i, err)
	}
	return m
}

// The stable system tier and tool menu an agent run sends every turn (the SDK
// composes them once per run; ENG-126 keeps them fixed across a conversation).
var (
	agentSystem = []*pb.SystemBlock{{Text: "<System>you are the assistant; rules ...</System>\n<ActionProtocol>tools, skills ...</ActionProtocol>", Cache: true}}
	agentTools  = []*pb.ToolDefinitionProto{
		{Name: "final_answer", Description: "Reply.", ParametersJson: `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`},
		{Name: "mcp_erp_invoices", Description: "List invoices.", ParametersJson: `{"type":"object","properties":{"customer":{"type":"string"}}}`},
	}
)

func TestPromptCacheE2E_TwoTurnsShareACachedPrefixAndTheSavingIsBooked(t *testing.T) {
	t.Parallel()
	api := &cachingMessagesAPI{}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(srv.Close)

	provider, err := llm.NewProvider(config.LLMProviderConfig{
		Default: "claude",
		Generators: []config.GeneratorConfig{{ID: "claude", Provider: "anthropic", Model: "claude-x",
			Endpoint: srv.URL + "/v1", TimeoutMs: 5000, NativeTools: true}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, meter)
	gw.SetStepModels(provider)
	s := &Server{LLMGateway: gw, SpendMeter: meter}
	lease, err := gw.Acquire(context.Background(),
		domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:claude"}}, 1<<20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s-cache", StepIndex: 0, AgentID: "chat"})
	ctx := ctxWithMD("x-agent-id", "chat")

	// Turn 2 replays turn 1 and adds the reply and the next message, as a
	// conversation does.
	conversations := [][]*pb.ModelMessageProto{
		{{Role: "user", Content: "what does ACME owe?"}},
		{
			{Role: "user", Content: "what does ACME owe?"},
			{Role: "assistant", Content: "ACME owes 120 EUR."},
			{Role: "user", Content: "and BETA?"},
		},
	}
	var usages []*pb.GenerateUsage
	for i, msgs := range conversations {
		resp, err := s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
			LeaseId: string(lease), System: agentSystem, Messages: msgs, Tools: agentTools,
			CacheTtl: pb.CacheTTL_CACHE_TTL_LONG,
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		usages = append(usages, resp.GetUsage())
	}

	// 1. The cacheable prefix (tools, then system) is byte-identical on the wire.
	first, second := api.body(t, 0), api.body(t, 1)
	for _, k := range []string{"tools", "system", "model"} {
		if string(first[k]) != string(second[k]) {
			t.Errorf("%s differs between turns:\n%s\n%s", k, first[k], second[k])
		}
	}
	// 2. The system tier carries the breakpoint with the long (1h) TTL.
	var system []map[string]any
	if err := json.Unmarshal(first["system"], &system); err != nil || len(system) != 1 {
		t.Fatalf("system = %s (%v)", first["system"], err)
	}
	cc, _ := system[0]["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Errorf("system cache_control = %v, want ephemeral 1h", system[0]["cache_control"])
	}
	// 3. Turn 2's history carries rolling breakpoints (the Hermes layout), within
	// Anthropic's four.
	var msgs []map[string]any
	_ = json.Unmarshal(second["messages"], &msgs)
	marks := 1 // the system one
	for _, m := range msgs {
		blocks, _ := m["content"].([]any)
		for _, b := range blocks {
			if _, ok := b.(map[string]any)["cache_control"]; ok {
				marks++
			}
		}
	}
	if marks < 2 || marks > 4 {
		t.Errorf("turn 2 sends %d breakpoints, want the system one plus rolling ones, at most 4", marks)
	}

	// 4. The agent sees the cache breakdown per turn.
	if usages[0].GetCacheWriteTokens() != cachedPrefixTokens || usages[0].GetCacheReadTokens() != 0 {
		t.Errorf("turn 1 usage = %+v, want a cache write", usages[0])
	}
	if usages[1].GetCacheReadTokens() != cachedPrefixTokens || usages[1].GetCacheWriteTokens() != 0 {
		t.Errorf("turn 2 usage = %+v, want a cache read", usages[1])
	}

	// 5. The spend ledger books both: the session, the lease and the Execute metadata.
	sp := meter.Session("s-cache")
	if sp.Calls != 2 || sp.CacheReadTokens != cachedPrefixTokens || sp.CacheWriteTokens != cachedPrefixTokens {
		t.Errorf("session spend = %+v, want 2 calls, %d cache read and %d cache write", sp, cachedPrefixTokens, cachedPrefixTokens)
	}
	wantIn := 2*40 + 2*cachedPrefixTokens // input includes cache reads and writes
	if sp.InputTokens != wantIn {
		t.Errorf("session input = %d, want %d", sp.InputTokens, wantIn)
	}
	meta := map[string]string{}
	s.stampSpend(meta, "s-cache", llm.SessionSpend{})
	if meta[MetaLLMCacheReadTokens] != fmt.Sprint(cachedPrefixTokens) || meta[MetaLLMCacheWriteTokens] != fmt.Sprint(cachedPrefixTokens) {
		t.Errorf("execute metadata = %v", meta)
	}
	u, err := gw.Complete(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if u.CacheReadTokens != cachedPrefixTokens || u.CacheWriteTokens != cachedPrefixTokens {
		t.Errorf("lease usage = %+v", u)
	}
}
