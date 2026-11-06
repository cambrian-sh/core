package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// T-20261106-post-model-hooks-never-see-native-tool-call: post_model hooks see the
// whole reply — text and tool calls — and a rewrite of the native tool lane's
// deliverable (the final-answer call's answer argument) reaches the caller on
// every Provider entry point that runs post_model.

const secret = "sk-e2e-AAAAAAAAAAAAAAAAAAAA"

// openAIToolReply is a unary chat completion carrying narration, a non-final call
// and the final-answer call. finalArgs is the final call's raw argument JSON.
func openAIToolReply(finalArgs string) string {
	enc := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":` +
		enc("narration "+secret) + `,"tool_calls":[` +
		`{"id":"call_0","type":"function","function":{"name":"write_file","arguments":` + enc(`{"body":"`+secret+`"}`) + `}},` +
		`{"id":"call_1","type":"function","function":{"name":"final_answer","arguments":` + enc(finalArgs) + `}}` +
		`]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
}

// openAIToolReplySSE is the same reply streamed: the final answer's argument JSON
// arrives in fragments, so the hook must see the ASSEMBLED call.
var openAIToolReplySSE = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"content":"narration ` + secret + `"},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"final_answer","arguments":"{\"answer\":\"the key is sk-e2e-"}}]},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"AAAAAAAAAAAAAAAAAAAA\",\"type\":\"text\"}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// redactSecret is redact_output's post_model behaviour, as a plugin would write it.
func redactSecret() domain.Hook {
	return modelHook{domain.HookSpec{Name: "redact", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true},
		func(ev domain.HookEvent) domain.HookResult {
			var res domain.HookResult
			if strings.Contains(ev.Text, secret) {
				t := strings.ReplaceAll(ev.Text, secret, "[REDACTED]")
				res.Text = &t
			}
			if fa := ev.FinalAnswer; fa != nil && strings.Contains(fa.Value, secret) {
				a := strings.ReplaceAll(fa.Value, secret, "[REDACTED]")
				res.FinalAnswer = &a
			}
			return res
		}}
}

func chainOf(t *testing.T, hs ...domain.Hook) *domain.HookChain {
	t.Helper()
	c, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func finalArg(t *testing.T, calls []domain.ModelToolCall, name, arg string) string {
	t.Helper()
	for _, c := range calls {
		if c.Name == name {
			s, err := domain.ToolCallStringArg(c.Arguments, arg)
			if err != nil {
				t.Fatalf("%s args %s: %v", name, c.Arguments, err)
			}
			return s
		}
	}
	t.Fatalf("no %s call in %+v", name, calls)
	return ""
}

func TestPostModel_RedactsTheFinalAnswerOnEveryUnaryEntryPoint(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, openAIToolReply(`{"answer":"the key is `+secret+`","type":"text"}`))
	p := sseProvider(t, chainOf(t, redactSecret()), srv.URL)

	check := func(lane, text string, calls []domain.ModelToolCall) {
		t.Helper()
		if strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED]") {
			t.Errorf("%s: text = %q", lane, text)
		}
		if got := finalArg(t, calls, "final_answer", "answer"); got != "the key is [REDACTED]" {
			t.Errorf("%s: final answer = %q", lane, got)
		}
		if got := finalArg(t, calls, "final_answer", "type"); got != "text" {
			t.Errorf("%s: the other fields of the final call must survive: type = %q", lane, got)
		}
		// A non-final call is the ToolExecutor's business (pre_tool hooks), untouched here.
		if string(calls[0].Arguments) != `{"body":"`+secret+`"}` {
			t.Errorf("%s: a non-final call was rewritten: %s", lane, calls[0].Arguments)
		}
	}

	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	check("GenerateOn", resp.Text, resp.ToolCalls)

	resp, err = p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).GenerateRequest(context.Background(), toolReq())
	if err != nil {
		t.Fatal(err)
	}
	check("generateRequest", resp.Text, resp.ToolCalls)

	turn, err := p.GeneratorFor(domain.PurposePlanner).(domain.ToolCallingGenerator).
		GenerateWithTools(context.Background(), toolReq().Messages, toolReq().Tools)
	if err != nil {
		t.Fatal(err)
	}
	check("GenerateWithTools", turn.Text, turn.ToolCalls)
}

func TestPostModel_ObservedLaneRedactsTheAssembledFinalAnswer(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, openAIToolReplySSE)
	p := sseProvider(t, chainOf(t, redactSecret()), srv.URL)
	var live strings.Builder
	resp, _, err := p.GenerateOnObserved(context.Background(), []string{"a"}, toolReq(), func(d domain.ModelStreamDelta) {
		live.WriteString(d.Text)
		if d.ToolCall != nil {
			live.WriteString(d.ToolCall.ArgumentsDelta)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := finalArg(t, resp.ToolCalls, "final_answer", "answer"); got != "the key is [REDACTED]" {
		t.Fatalf("final answer = %q", got)
	}
	if strings.Contains(resp.Text, secret) {
		t.Fatalf("text = %q", resp.Text)
	}
	// The Provider reports raw deltas by contract; the GATEWAY suppresses them when
	// a post_model hook exists (network.TestGenerateWithTools_FinalAnswerSecret…).
	if !strings.Contains(live.String(), "sk-e2e-") {
		t.Fatalf("observe saw %q", live.String())
	}
}

func TestPostModel_DenyOnTheFinalAnswerRefusesTheWholeReply(t *testing.T) {
	t.Parallel()
	deny := modelHook{domain.HookSpec{Name: "dlp", Points: []domain.HookPoint{domain.HookPostModel}},
		func(ev domain.HookEvent) domain.HookResult {
			if ev.FinalAnswer != nil && strings.Contains(ev.FinalAnswer.Value, "sk-e2e-") {
				return domain.HookResult{Verdict: domain.HookDeny, Reason: "secret in the answer"}
			}
			return domain.HookResult{}
		}}
	srv, _ := captureServer(t, openAIToolReply(`{"answer":"`+secret+`"}`))
	p := sseProvider(t, chainOf(t, deny), srv.URL)
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if !errors.Is(err, domain.ErrHookDenied) || resp.Text != "" || resp.ToolCalls != nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	sse, _ := captureServer(t, openAIToolReplySSE)
	p = sseProvider(t, chainOf(t, deny), sse.URL)
	resp, _, err = p.GenerateOnObserved(context.Background(), []string{"a"}, toolReq(), func(domain.ModelStreamDelta) {})
	if !errors.Is(err, domain.ErrHookDenied) || resp.ToolCalls != nil {
		t.Fatalf("observed: resp=%+v err=%v", resp, err)
	}
}

func TestPostModel_UnparseableFinalAnswerUnderARedactionFailsClosed(t *testing.T) {
	t.Parallel()
	// on_failure: allow, explicitly — a redaction that cannot be applied still denies.
	h := redactSecret().(modelHook)
	h.spec.OnFailure = domain.HookFailAllow
	srv, _ := captureServer(t, openAIToolReply(`{"answer":"`+secret)) // truncated JSON
	p := sseProvider(t, chainOf(t, h), srv.URL)
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if !errors.Is(err, domain.ErrHookDenied) || !strings.Contains(err.Error(), "cannot be rewritten") || resp.ToolCalls != nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestPostModel_NoHookPathIsUnchanged(t *testing.T) {
	t.Parallel()
	body := openAIToolReply(`{ "type" : "text", "answer" : "<b>` + secret + `</b>" }`)
	srvA, _ := captureServer(t, body)
	srvB, _ := captureServer(t, body)
	bare := sseProvider(t, nil, srvA.URL)
	// A hook that acts on nothing: the post_model path runs, and must change nothing.
	idle := modelHook{domain.HookSpec{Name: "idle", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true},
		func(domain.HookEvent) domain.HookResult { return domain.HookResult{} }}
	hooked := sseProvider(t, chainOf(t, idle), srvB.URL)

	want, _, err := bare.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	// Golden: the arguments are byte-for-byte the model's, whitespace and order kept.
	if got := string(want.ToolCalls[1].Arguments); got != `{ "type" : "text", "answer" : "<b>`+secret+`</b>" }` {
		t.Fatalf("no-hook args = %s", got)
	}
	got, _, err := hooked.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.ToolCalls, got.ToolCalls) || want.Text != got.Text {
		t.Fatalf("an idle hook changed the reply:\nwant %+v\ngot  %+v", want, got)
	}
}

func TestPostModel_FinalAnswerToolIsConfigured(t *testing.T) {
	t.Parallel()
	// The deliverable is execution.step_output.final_answer_tool/arg: here "write_file"
	// / "body", so the literal final_answer call is just another call.
	srv, _ := captureServer(t, openAIToolReply(`{"answer":"`+secret+`"}`))
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "a",
		Generators: []config.GeneratorConfig{{ID: "a", Provider: "openai", Model: "m", Endpoint: srv.URL,
			TimeoutMs: 5000, NativeTools: true}},
		Health: config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, WithHooks(chainOf(t, redactSecret())), WithFinalAnswer("write_file", "body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, toolReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := finalArg(t, resp.ToolCalls, "write_file", "body"); got != "[REDACTED]" {
		t.Errorf("configured deliverable = %q", got)
	}
	if got := finalArg(t, resp.ToolCalls, "final_answer", "answer"); got != secret {
		t.Errorf("the unconfigured final_answer was treated as the deliverable: %q", got)
	}
}
