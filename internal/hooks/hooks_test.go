package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// The command-hook tests re-exec this test binary as the hook program. The mode
// travels in HOOKTEST_MODE, which reaches the child only because the hook names it
// in its env passthrough — the same allowlist every command hook gets.
func TestMain(m *testing.M) {
	if mode := os.Getenv("HOOKTEST_MODE"); mode != "" {
		os.Exit(helperProcess(mode))
	}
	os.Exit(m.Run())
}

func helperProcess(mode string) int {
	in, _ := io.ReadAll(os.Stdin)
	var ev domain.HookEvent
	if err := json.Unmarshal(in, &ev); err != nil {
		fmt.Fprintln(os.Stderr, "bad event:", err)
		return 3
	}
	switch mode {
	case "deny":
		fmt.Printf(`{"verdict":"deny","reason":"tool %s is blocked"}`, ev.Tool)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "exit":
		fmt.Fprintln(os.Stderr, "nope")
		return 2
	case "garbage":
		fmt.Print(`{"verdct":"deny"}`)
	case "env":
		// Report whether a secret reached the child.
		if os.Getenv("HOOKTEST_SECRET_TOKEN") != "" || os.Getenv("CAMBRIAN_SECRET_KEY") != "" {
			fmt.Print(`{"verdict":"deny","reason":"secret leaked"}`)
		}
	case "guarded":
		// Uppercase every guarded argument the event lists (command-hook JSON both ways).
		var rw []domain.HookArgRewrite
		for _, g := range ev.GuardedArgs {
			rw = append(rw, domain.HookArgRewrite{Call: g.Call, Arg: g.Arg, Value: strings.ToUpper(g.Value)})
		}
		b, _ := json.Marshal(domain.HookResult{GuardedArgs: rw})
		fmt.Print(string(b))
	case "empty":
	}
	return 0
}

func selfArgv() []string { return []string{os.Args[0], "-test.run=^$"} }

func commandHook(t *testing.T, mode string, timeout time.Duration, extraEnv ...string) *Command {
	t.Helper()
	t.Setenv("HOOKTEST_MODE", mode)
	h, err := NewCommand(domain.HookSpec{Name: "cmd-" + mode, Points: []domain.HookPoint{domain.HookPreTool}, Timeout: timeout},
		selfArgv(), append([]string{"HOOKTEST_MODE"}, extraEnv...))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func runChain(t *testing.T, h domain.Hook, ev domain.HookEvent) domain.HookOutcome {
	t.Helper()
	c, err := domain.NewHookChain([]domain.Hook{h}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Run(context.Background(), ev)
}

func TestCommandHook_AnswersFromStdout(t *testing.T) {
	out := runChain(t, commandHook(t, "deny", 20*time.Second), domain.HookEvent{Point: domain.HookPreTool, Tool: "web"})
	if out.Verdict != domain.HookDeny || out.Reason != "tool web is blocked" {
		t.Fatalf("outcome = %+v", out)
	}
	if out := runChain(t, commandHook(t, "empty", 20*time.Second), domain.HookEvent{Point: domain.HookPreTool}); out.Verdict != domain.HookAllow {
		t.Fatalf("empty stdout should continue: %+v", out)
	}
}

func TestCommandHook_TimeoutKillsAndFailsClosed(t *testing.T) {
	start := time.Now()
	out := runChain(t, commandHook(t, "sleep", 300*time.Millisecond), domain.HookEvent{Point: domain.HookPreTool})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced: %s", elapsed)
	}
	if out.Verdict != domain.HookDeny || len(out.Invocations) != 1 || out.Invocations[0].Err == "" {
		t.Fatalf("a timed-out pre_tool command must deny (on_failure default block): %+v", out)
	}
}

func TestCommandHook_FailuresAreFailures(t *testing.T) {
	for _, mode := range []string{"exit", "garbage"} {
		out := runChain(t, commandHook(t, mode, 20*time.Second), domain.HookEvent{Point: domain.HookPreTool})
		if out.Verdict != domain.HookDeny || out.Invocations[0].Err == "" {
			t.Errorf("%s: %+v", mode, out)
		}
	}
}

func TestCommandHook_GetsNoSecrets(t *testing.T) {
	t.Setenv("HOOKTEST_SECRET_TOKEN", "x")
	t.Setenv("CAMBRIAN_SECRET_KEY", "y")
	// Even NAMED in the passthrough, a secret-shaped name is stripped.
	h := commandHook(t, "env", 20*time.Second, "HOOKTEST_SECRET_TOKEN", "CAMBRIAN_SECRET_KEY")
	if out := runChain(t, h, domain.HookEvent{Point: domain.HookPreTool}); out.Verdict != domain.HookAllow {
		t.Fatalf("a secret reached the command hook: %+v", out)
	}
}

func TestBuiltins_FromConfig(t *testing.T) {
	t.Parallel()
	hs, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "no-rm", Kind: config.HookKindDenyArgsRegex, Pattern: `rm\s+-rf`, Tools: []string{"execute_*"}, Reason: "no rm -rf"},
		{Name: "ask-web", Kind: config.HookKindRequireApproval, Tools: []string{"mcp:*"}, Priority: 5},
		{Name: "redact", Kind: config.HookKindRedactOutput, Pattern: `sk-[A-Za-z0-9]+`, MayRewrite: true, Points: []string{"post_tool", "post_model"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if out := c.Run(ctx, domain.HookEvent{Point: domain.HookPreTool, Tool: "execute_command", Args: json.RawMessage(`{"cmd":"rm -rf /"}`)}); out.Verdict != domain.HookDeny || out.Reason != "no rm -rf" {
		t.Errorf("deny_args_regex: %+v", out)
	}
	if out := c.Run(ctx, domain.HookEvent{Point: domain.HookPreTool, Tool: "read_file", Args: json.RawMessage(`{"cmd":"rm -rf /"}`)}); out.Verdict != domain.HookAllow {
		t.Errorf("deny_args_regex outside its tools: %+v", out)
	}
	if out := c.Run(ctx, domain.HookEvent{Point: domain.HookPreTool, Tool: "mcp:fs/write", Args: json.RawMessage(`{}`)}); out.Verdict != domain.HookAsk {
		t.Errorf("require_approval over mcp:*: %+v", out)
	}
	out := c.Run(ctx, domain.HookEvent{Point: domain.HookPostTool, Tool: "web", Result: json.RawMessage(`{"a":["key sk-abc123"],"sk-key":1}`)})
	if !out.ResultRewritten || string(out.Result) != `{"a":["key [REDACTED]"],"sk-key":1}` {
		t.Errorf("redact post_tool: %s", out.Result)
	}
	out = c.Run(ctx, domain.HookEvent{Point: domain.HookPostModel, Text: "use sk-zzz"})
	if !out.TextRewritten || out.Text != "use [REDACTED]" {
		t.Errorf("redact post_model: %q", out.Text)
	}
}

func TestFromConfig_RejectsKindsAtTheWrongPoint(t *testing.T) {
	t.Parallel()
	_, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "x", Kind: config.HookKindRequireApproval, Tools: []string{"*"}, Points: []string{"post_tool"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "cannot attach") {
		t.Fatalf("err = %v", err)
	}
	if _, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "r", Kind: config.HookKindRedactOutput, Pattern: "x"}, // no may_rewrite
	}}); err == nil {
		t.Fatal("redact_output without may_rewrite accepted")
	}
}

func TestHooksConfig_Validate(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	if err := cfg.Execution.Validate(); err != nil && strings.Contains(err.Error(), "hooks") {
		t.Fatalf("defaults: %v", err)
	}
	cfg.Execution.Hooks.Entries = []config.HookConfig{
		{Name: "a", Kind: "lol"},
		{Name: "a", Kind: config.HookKindDenyArgsRegex, Pattern: "("},
		{Name: "c", Kind: config.HookKindCommand},
		{Name: "d", Kind: config.HookKindRequireApproval, Tools: []string{"x"}, OnFailure: "maybe", Points: []string{"pre_lunch"}},
	}
	err := cfg.Execution.Validate()
	if err == nil {
		t.Fatal("bad hooks accepted")
	}
	for _, want := range []string{"kind must be one of", "declared twice", "pattern does not compile", "command is required", "points is required", "on_failure", "unknown point"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error lacks %q: %v", want, err)
		}
	}
}

// T-20261106-post-model-hooks-never-see-native-tool-call: at post_model,
// redact_output redacts the native tool lane's deliverable (the final-answer
// call's argument) as well as the text, and leaves other calls' arguments alone.
func TestRedactOutput_PostModelRedactsTheFinalAnswer(t *testing.T) {
	t.Parallel()
	hs, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "redact", Kind: config.HookKindRedactOutput, Pattern: `sk-[A-Za-z0-9-]+`, MayRewrite: true, Points: []string{"post_model"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := domain.HookEvent{Point: domain.HookPostModel, Text: "I found sk-text-1"}
	ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView([]domain.ModelToolCall{
		{ID: "c0", Name: "write_file", Arguments: []byte(`{"body":"sk-other-2"}`)},
		{ID: "c1", Name: "final_answer", Arguments: []byte(`{"answer":"the key is sk-e2e-3","type":"text"}`)},
	}, "final_answer", "answer")
	out := c.Run(context.Background(), ev)
	if out.Verdict != domain.HookAllow || out.Text != "I found [REDACTED]" || out.FinalAnswer != "the key is [REDACTED]" {
		t.Fatalf("outcome = %+v", out)
	}
	if got := string(out.ToolCalls[1].Args); got != `{"answer":"the key is [REDACTED]","type":"text"}` {
		t.Errorf("final-answer args = %s", got)
	}
	if got := string(out.ToolCalls[0].Args); got != `{"body":"sk-other-2"}` {
		t.Errorf("a non-final call's args were rewritten: %s", got)
	}

	// A final answer with nothing to redact is left exactly as the model wrote it.
	ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView([]domain.ModelToolCall{
		{ID: "c1", Name: "final_answer", Arguments: []byte(`{ "answer" : "clean" }`)},
	}, "final_answer", "answer")
	ev.Text = ""
	if out := c.Run(context.Background(), ev); out.FinalAnswerRewritten || out.TextRewritten {
		t.Fatalf("clean reply rewritten: %+v", out)
	}
}

// T-20261106-post-model-hooks-do-not-guard-control-verb: redact_output at post_model
// redacts the guarded control-verb arguments (yield_subgoal's intent,
// spawn_subagent's objective) with the configured defaults, and leaves an
// unguarded call alone.
func TestRedactOutput_PostModelRedactsGuardedControlVerbArgs(t *testing.T) {
	t.Parallel()
	hs, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "redact", Kind: config.HookKindRedactOutput, Pattern: `sk-[A-Za-z0-9-]+`, MayRewrite: true, Points: []string{"post_model"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := []domain.ModelToolCall{
		{ID: "c0", Name: "yield_subgoal", Arguments: []byte(`{"intent":"fetch with sk-sub-1","capability_hint":"web"}`)},
		{ID: "c1", Name: "spawn_subagent", Arguments: []byte(`{"objective":"use sk-obj-2 to log in","mode":"isolated"}`)},
		{ID: "c2", Name: "write_file", Arguments: []byte(`{"body":"sk-other-3"}`)},
	}
	ev := domain.HookEvent{Point: domain.HookPostModel}
	ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView(calls, "final_answer", "answer")
	ev.GuardedArgs = domain.HookGuardedView(calls, "final_answer", "answer", config.HooksConfig{}.GuardedArgumentList())
	out := c.Run(context.Background(), ev)
	if out.Verdict != domain.HookAllow || !out.GuardedArgsRewritten {
		t.Fatalf("outcome = %+v", out)
	}
	var y, s map[string]string
	_ = json.Unmarshal(out.ToolCalls[0].Args, &y)
	_ = json.Unmarshal(out.ToolCalls[1].Args, &s)
	if y["intent"] != "fetch with [REDACTED]" || y["capability_hint"] != "web" {
		t.Errorf("yield_subgoal args = %s", out.ToolCalls[0].Args)
	}
	if s["objective"] != "use [REDACTED] to log in" || s["mode"] != "isolated" {
		t.Errorf("spawn_subagent args = %s", out.ToolCalls[1].Args)
	}
	if got := string(out.ToolCalls[2].Args); got != `{"body":"sk-other-3"}` {
		t.Errorf("an unguarded call was rewritten: %s", got)
	}

	// An unreadable guarded argument fails closed: the reply is refused.
	bad := []domain.ModelToolCall{{ID: "c0", Name: "spawn_subagent", Arguments: []byte(`{"objective": 42}`)}}
	ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView(bad, "final_answer", "answer")
	ev.GuardedArgs = domain.HookGuardedView(bad, "final_answer", "answer", config.HooksConfig{}.GuardedArgumentList())
	if out := c.Run(context.Background(), ev); out.Verdict != domain.HookDeny {
		t.Fatalf("unreadable objective: %+v", out)
	}
}

// T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or: agents/purposes reach
// the spec from config; a scoped redaction leaves the planner's reply untouched.
func TestFromConfig_ThreadsAgentAndPurposeScope(t *testing.T) {
	t.Parallel()
	hs, err := FromConfig(config.HooksConfig{Entries: []config.HookConfig{
		{Name: "redact-steps", Kind: config.HookKindRedactOutput, Pattern: `CODE-[0-9]+`, MayRewrite: true,
			Points: []string{"post_model"}, Agents: []string{"research*"}, Purposes: []string{"agent_step"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sp := hs[0].Spec(); len(sp.Agents) != 1 || sp.Agents[0] != "research*" || len(sp.Purposes) != 1 || sp.Purposes[0] != "agent_step" {
		t.Fatalf("spec = %+v", sp)
	}
	c, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	planner := domain.HookEvent{Point: domain.HookPostModel, Model: &domain.HookModelCall{Purpose: "planner"}, Text: "plan: say CODE-123"}
	if out := c.Run(context.Background(), planner); out.TextRewritten || out.Text != "plan: say CODE-123" || len(out.Invocations) != 0 {
		t.Fatalf("planner reply touched: %+v", out)
	}
	step := domain.HookEvent{Point: domain.HookPostModel, AgentID: "researcher", Model: &domain.HookModelCall{Purpose: "agent_step"}, Text: "CODE-123"}
	if out := c.Run(context.Background(), step); out.Text != "[REDACTED]" {
		t.Fatalf("agent step not redacted: %+v", out)
	}
	other := step
	other.AgentID = "coder"
	if out := c.Run(context.Background(), other); out.TextRewritten {
		t.Fatalf("other agent's step redacted: %+v", out)
	}
}

// A command hook receives the guarded arguments in its JSON event and may answer
// guarded_args.
func TestCommandHook_SeesAndRewritesGuardedArgs(t *testing.T) {
	t.Setenv("HOOKTEST_MODE", "guarded")
	h, err := NewCommand(domain.HookSpec{Name: "cmd-guarded", Points: []domain.HookPoint{domain.HookPostModel},
		MayRewrite: true, Timeout: 20 * time.Second}, selfArgv(), []string{"HOOKTEST_MODE"})
	if err != nil {
		t.Fatal(err)
	}
	calls := []domain.ModelToolCall{{ID: "c0", Name: "yield_subgoal", Arguments: []byte(`{"intent":"quiet"}`)}}
	ev := domain.HookEvent{Point: domain.HookPostModel}
	ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView(calls, "final_answer", "answer")
	ev.GuardedArgs = domain.HookGuardedView(calls, "final_answer", "answer", config.HooksConfig{}.GuardedArgumentList())
	out := runChain(t, h, ev)
	if out.Verdict != domain.HookAllow || !out.GuardedArgsRewritten || string(out.ToolCalls[0].Args) != `{"intent":"QUIET"}` {
		t.Fatalf("outcome = %+v", out)
	}
}
