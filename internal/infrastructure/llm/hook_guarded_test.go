package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/hooks"
)

// T-20261106-post-model-hooks-do-not-guard-control-verb: post_model guards the
// configured control-verb arguments as it guards the final answer, and
// T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or: a post_model hook runs
// only for the calls its static scope covers.

// openAIReplyWithCalls is a unary chat completion carrying text and the given
// calls (name, raw argument JSON).
func openAIReplyWithCalls(text string, calls ...[2]string) string {
	enc := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	parts := make([]string, 0, len(calls))
	for i, c := range calls {
		parts = append(parts, `{"id":"call_`+string(rune('0'+i))+`","type":"function","function":{"name":`+enc(c[0])+`,"arguments":`+enc(c[1])+`}}`)
	}
	return `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":` +
		enc(text) + `,"tool_calls":[` + strings.Join(parts, ",") + `]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
}

func builtinRedact(t *testing.T, agents, purposes []string) domain.Hook {
	t.Helper()
	h, err := hooks.NewRedactOutput(domain.HookSpec{Name: "redact", Points: []domain.HookPoint{domain.HookPostModel},
		MayRewrite: true, Agents: agents, Purposes: purposes}, nil, `sk-e2e-[A-Z]+`, "")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func controlVerbReply() string {
	return openAIReplyWithCalls("delegating",
		[2]string{"yield_subgoal", `{"intent":"look up ` + secret + `","capability_hint":"web"}`},
		[2]string{"spawn_subagent", `{"objective":"log in with ` + secret + `","mode":"isolated"}`},
		[2]string{"write_file", `{"body":"` + secret + `"}`})
}

func TestPostModel_RedactOutputGuardsControlVerbArguments(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, controlVerbReply())
	p := sseProvider(t, chainOf(t, builtinRedact(t, nil, nil)), srv.URL)
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := finalArg(t, resp.ToolCalls, "yield_subgoal", "intent"); got != "look up [REDACTED]" {
		t.Errorf("yield_subgoal intent = %q", got)
	}
	if got := finalArg(t, resp.ToolCalls, "yield_subgoal", "capability_hint"); got != "web" {
		t.Errorf("other fields must survive: %q", got)
	}
	if got := finalArg(t, resp.ToolCalls, "spawn_subagent", "objective"); got != "log in with [REDACTED]" {
		t.Errorf("spawn_subagent objective = %q", got)
	}
	// Not guarded: the ToolExecutor's pre_tool hooks own it.
	if got := finalArg(t, resp.ToolCalls, "write_file", "body"); got != secret {
		t.Errorf("unguarded call rewritten: %q", got)
	}
}

func TestPostModel_GuardedArgumentsAreConfigured(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, controlVerbReply())
	// The operator's list replaces the defaults: only write_file.body is guarded now.
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "a",
		Generators: []config.GeneratorConfig{{ID: "a", Provider: "openai", Model: "m", Endpoint: srv.URL,
			TimeoutMs: 5000, NativeTools: true}},
		Health: config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, WithHooks(chainOf(t, builtinRedact(t, nil, nil))),
		WithGuardedArgs(config.HooksConfig{GuardedArguments: []config.GuardedArgumentConfig{{Tool: "write_file", Arg: "body"}}}.GuardedArgumentList()))
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := finalArg(t, resp.ToolCalls, "write_file", "body"); got != "[REDACTED]" {
		t.Errorf("configured guard: %q", got)
	}
	if got := finalArg(t, resp.ToolCalls, "spawn_subagent", "objective"); got != "log in with "+secret {
		t.Errorf("default guard still applied after replacement: %q", got)
	}
}

func TestPostModel_DenyOnAGuardedArgumentRefusesTheReply(t *testing.T) {
	t.Parallel()
	deny := modelHook{domain.HookSpec{Name: "dlp", Points: []domain.HookPoint{domain.HookPostModel}},
		func(ev domain.HookEvent) domain.HookResult {
			for _, g := range ev.GuardedArgs {
				if g.Tool == "yield_subgoal" && strings.Contains(g.Value, "sk-e2e-") {
					return domain.HookResult{Verdict: domain.HookDeny, Reason: "secret in a subgoal"}
				}
			}
			return domain.HookResult{}
		}}
	srv, _ := captureServer(t, controlVerbReply())
	p := sseProvider(t, chainOf(t, deny), srv.URL)
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if !errors.Is(err, domain.ErrHookDenied) || resp.ToolCalls != nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestPostModel_UnreadableGuardedArgumentFailsClosed(t *testing.T) {
	t.Parallel()
	h := builtinRedact(t, nil, nil)
	srv, _ := captureServer(t, openAIReplyWithCalls("x", [2]string{"spawn_subagent", `{"objective":["` + secret + `"]}`}))
	p := sseProvider(t, chainOf(t, h), srv.URL)
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if !errors.Is(err, domain.ErrHookDenied) || !strings.Contains(err.Error(), "spawn_subagent.objective") || resp.ToolCalls != nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestPostModel_ScopedHookRunsOnlyForItsCalls(t *testing.T) {
	t.Parallel()
	body := openAIReplyWithCalls("plan with "+secret, [2]string{"final_answer", `{"answer":"` + secret + `"}`})
	srv, _ := captureServer(t, body)
	// Agent steps of research* only: the planner organ (no agent, purpose planner)
	// and another agent's step are untouched.
	p := sseProvider(t, chainOf(t, builtinRedact(t, []string{"research*"}, []string{"agent_step"})), srv.URL)

	resp, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).GenerateRequest(context.Background(), toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plan with "+secret {
		t.Errorf("planner reply rewritten: %q", resp.Text)
	}
	researcher := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("researcher"))
	resp, _, err = p.GenerateOn(researcher, []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plan with [REDACTED]" || finalArg(t, resp.ToolCalls, "final_answer", "answer") != "[REDACTED]" {
		t.Errorf("researcher step not redacted: %+v", resp)
	}
	coder := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("coder"))
	resp, _, err = p.GenerateOn(coder, []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plan with "+secret {
		t.Errorf("another agent's step rewritten: %q", resp.Text)
	}
}
