package llm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// T-20261106-text-protocol-agent-replies-never-reach-post: a streamed call that a
// D11 post_model hook's scope matches is served BUFFERED — post_model sees the
// complete reply and only its (possibly rewritten) text is delivered. A streamed
// call no post_model hook matches streams live, unchanged.

const openAITextSSE = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[{"index":0,"delta":{"content":"the secret "},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[{"index":0,"delta":{"content":"is out"},"finish_reason":"stop"}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m-served","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`

// streamShape is a stream as "text|text|FINAL" plus its final Response.
func streamShape(t *testing.T, ch <-chan domain.StreamChunk) (string, *domain.GenerateResponse, domain.StreamChunk) {
	t.Helper()
	var parts []string
	var final *domain.GenerateResponse
	var last domain.StreamChunk
	for c := range ch {
		if c.Response != nil {
			final = c.Response
		}
		switch {
		case c.IsFinal:
			parts = append(parts, "FINAL")
			last = c
		case c.Text != "":
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "|"), final, last
}

func scopedPostModel(name string, agents, purposes []string, run func(ev domain.HookEvent) domain.HookResult) domain.Hook {
	return modelHook{domain.HookSpec{Name: name, Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true,
		Agents: agents, Purposes: purposes}, run}
}

func redactSecretText(ev domain.HookEvent) domain.HookResult {
	s := strings.ReplaceAll(ev.Text, "secret", "[R]")
	return domain.HookResult{Text: &s}
}

func agentCtx(id string) context.Context {
	return domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id))
}

// The golden live shape: the provider's text chunks in order, then the final chunk.
const liveShape = "the secret |is out|FINAL"

func TestStreamOn_NoHookStreamsLive(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, openAITextSSE)
	p := sseProvider(t, nil, srv.URL)
	ch, served, err := p.StreamOn(agentCtx("a1"), []string{"a"}, domain.UserPrompt("go"))
	if err != nil || served != "a" {
		t.Fatalf("served %q, %v", served, err)
	}
	if shape, final, _ := streamShape(t, ch); shape != liveShape || final == nil || final.Text != "the secret is out" {
		t.Fatalf("shape %q final %+v", shape, final)
	}
}

func TestStreamOn_ScopedHookThatDoesNotMatchStreamsLive(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name             string
		agents, purposes []string
	}{
		{"another agent", []string{"other"}, nil},
		{"planner only", nil, []string{"planner"}},
	} {
		var ran atomic.Int32
		chain, err := domain.NewHookChain([]domain.Hook{scopedPostModel("s", c.agents, c.purposes,
			func(ev domain.HookEvent) domain.HookResult { ran.Add(1); return redactSecretText(ev) })}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv, _ := captureServer(t, openAITextSSE)
		p := sseProvider(t, chain, srv.URL)
		ch, _, err := p.StreamOn(agentCtx("a1"), []string{"a"}, domain.UserPrompt("go"))
		if err != nil {
			t.Fatal(err)
		}
		if shape, _, _ := streamShape(t, ch); shape != liveShape || ran.Load() != 0 {
			t.Errorf("%s: shape %q, hook ran %d times; a hook scoped away must leave the stream live", c.name, shape, ran.Load())
		}
	}
}

func TestStreamOn_MatchingRedactHookDeliversOnlyTheRewrittenReply(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name             string
		agents, purposes []string
	}{
		{"unscoped", nil, nil},
		{"this agent's steps", []string{"a*"}, []string{"agent_step"}},
	} {
		var sawStreamed atomic.Bool
		chain, err := domain.NewHookChain([]domain.Hook{scopedPostModel("s", c.agents, c.purposes,
			func(ev domain.HookEvent) domain.HookResult {
				if ev.Model != nil && ev.Model.Streamed {
					sawStreamed.Store(true)
				}
				return redactSecretText(ev)
			})}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv, _ := captureServer(t, openAITextSSE)
		p := sseProvider(t, chain, srv.URL)
		ch, served, err := p.StreamOn(agentCtx("a1"), []string{"a"}, domain.UserPrompt("go"))
		if err != nil || served != "a" {
			t.Fatalf("%s: served %q, %v", c.name, served, err)
		}
		shape, final, last := streamShape(t, ch)
		if shape != "the [R] is out|FINAL" {
			t.Fatalf("%s: shape %q; want the redacted text as one chunk, then the final chunk", c.name, shape)
		}
		if final == nil || final.Text != "the [R] is out" || final.StopReason != domain.StopEndTurn ||
			final.Model != "m-served" || final.Usage.Input != 10 || final.Usage.Output != 5 {
			t.Fatalf("%s: the final chunk's C1 metadata must survive: %+v", c.name, final)
		}
		if last.UsageTotalTokens != 15 {
			t.Fatalf("%s: final chunk usage = %+v", c.name, last)
		}
		if sawStreamed.Load() {
			t.Errorf("%s: a buffered call is not a streamed call to the hook", c.name)
		}
	}
}

func TestStreamOn_PostModelDenyDeliversNothing(t *testing.T) {
	t.Parallel()
	chain, err := domain.NewHookChain([]domain.Hook{scopedPostModel("deny", nil, nil,
		func(ev domain.HookEvent) domain.HookResult {
			if strings.Contains(ev.Text, "secret") {
				return domain.HookResult{Verdict: domain.HookDeny, Reason: "pii"}
			}
			return domain.HookResult{}
		})}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := captureServer(t, openAITextSSE)
	p := sseProvider(t, chain, srv.URL)
	ch, _, err := p.StreamOn(agentCtx("a1"), []string{"a"}, domain.UserPrompt("go"))
	if !errors.Is(err, domain.ErrHookDenied) || ch != nil {
		t.Fatalf("ch=%v err=%v; a refusal returns an error and no stream", ch, err)
	}
}

func TestStreamOn_BufferedMidStreamBreakDeliversNothingPartial(t *testing.T) {
	t.Parallel()
	chain, err := domain.NewHookChain([]domain.Hook{scopedPostModel("s", nil, nil, redactSecretText)}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	broken := brokenStreamServer(t, &hits)
	p := sseProvider(t, chain, broken.URL)
	ch, _, err := p.StreamOn(agentCtx("a1"), []string{"a"}, domain.UserPrompt("go"))
	if err == nil || ch != nil {
		t.Fatalf("ch=%v err=%v; a broken buffered stream is an error with nothing forwarded", ch, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}

	// With a second candidate nothing was delivered yet, so the next one serves.
	good, _ := captureServer(t, openAITextSSE)
	p2 := sseProvider(t, chain, broken.URL, good.URL)
	ch, served, err := p2.StreamOn(agentCtx("a1"), []string{"a", "b"}, domain.UserPrompt("go"))
	if err != nil || served != "b" {
		t.Fatalf("served %q, %v", served, err)
	}
	if shape, _, _ := streamShape(t, ch); shape != "the [R] is out|FINAL" {
		t.Fatalf("shape %q; the abandoned attempt's partial text must not appear", shape)
	}
}

// The purpose handles' streams follow the same rule, decided by the purpose.
func TestPurposeStreams_FollowThePostModelScope(t *testing.T) {
	t.Parallel()
	chain, err := domain.NewHookChain([]domain.Hook{scopedPostModel("s", nil, []string{"planner"}, redactSecretText)}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := captureServer(t, openAITextSSE)
	p, err := NewProvider(config.LLMProviderConfig{
		Default:    "a",
		Generators: []config.GeneratorConfig{{ID: "a", Provider: "openai", Model: "m", Endpoint: srv.URL, TimeoutMs: 5000}},
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, WithHooks(chain))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// C1 StreamRequest: planner matches (buffered, redacted); verifier does not (live).
	ch, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).StreamRequest(ctx, domain.UserPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	if shape, final, _ := streamShape(t, ch); shape != "the [R] is out|FINAL" || final == nil || final.Text != "the [R] is out" {
		t.Fatalf("planner StreamRequest: shape %q final %+v", shape, final)
	}
	ch, err = p.GeneratorFor(domain.PurposeVerifier).(domain.RequestGenerator).StreamRequest(ctx, domain.UserPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	if shape, _, _ := streamShape(t, ch); shape != liveShape {
		t.Fatalf("verifier StreamRequest: shape %q; a hook scoped to the planner must leave it live", shape)
	}

	// Legacy GenerateStream on the matching purpose: buffered, redacted.
	sg, ok := p.GeneratorFor(domain.PurposePlanner).(interface {
		GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error)
	})
	if !ok {
		t.Fatal("purpose handle does not stream")
	}
	ch, err = sg.GenerateStream(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	finals := 0
	for c := range ch {
		text.WriteString(c.Text)
		if c.IsFinal {
			finals++
		}
	}
	if text.String() != "the [R] is out" || finals != 1 {
		t.Fatalf("legacy GenerateStream: text %q finals %d", text.String(), finals)
	}
}
