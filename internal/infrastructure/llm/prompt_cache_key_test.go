package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2133 (owner decision 2026-11-18): an opt-in, per-generator prompt_cache_key for
// OpenAI-compatible endpoints, OFF by default, derived deterministically from the
// cache-marked system prefix.

func cachedReq(system []domain.SystemBlock, user string) domain.GenerateRequest {
	return domain.GenerateRequest{System: system, Messages: []domain.ModelMessage{domain.UserMessage(user)}}
}

func TestPromptCacheKey_DerivedFromTheCacheMarkedPrefix(t *testing.T) {
	t.Parallel()
	on := &OpenAIClient{PromptCacheKey: true}
	prefix := []domain.SystemBlock{{Text: "persona"}, {Text: "skills menu", Cache: true}}

	k1 := on.cacheKeyFor(cachedReq(prefix, "turn one"))
	if !strings.HasPrefix(k1, promptCacheKeyPrefix) || len(k1) != len(promptCacheKeyPrefix)+32 {
		t.Fatalf("key = %q, want %q + 32 hex chars", k1, promptCacheKeyPrefix)
	}
	// Same prefix, different turn and an extra block AFTER the last mark: same key.
	tail := append(append([]domain.SystemBlock(nil), prefix...), domain.SystemBlock{Text: "today is Tuesday"})
	if k2 := on.cacheKeyFor(cachedReq(tail, "a different question")); k2 != k1 {
		t.Errorf("text after the last cache mark changed the key: %q vs %q", k2, k1)
	}
	// Any byte of the marked prefix changes it, including an unmarked block before the mark.
	if k := on.cacheKeyFor(cachedReq([]domain.SystemBlock{{Text: "persona!"}, {Text: "skills menu", Cache: true}}, "x")); k == k1 {
		t.Error("a change inside the cached prefix kept the key")
	}
	// Block boundaries are part of the identity: the same concatenated text split
	// differently is a different prefix.
	if k := on.cacheKeyFor(cachedReq([]domain.SystemBlock{{Text: "personaskills"}, {Text: " menu", Cache: true}}, "x")); k == k1 {
		t.Error("a different block split produced the same key")
	}
	// Deterministic across clients and calls (no clock, no instance state).
	if k := (&OpenAIClient{PromptCacheKey: true, Model: "other"}).cacheKeyFor(cachedReq(prefix, "y")); k != k1 {
		t.Errorf("key depends on something besides the prefix: %q vs %q", k, k1)
	}
	// No mark -> no key; switch off -> no key.
	if k := on.cacheKeyFor(cachedReq([]domain.SystemBlock{{Text: "persona"}}, "x")); k != "" {
		t.Errorf("unmarked request got key %q", k)
	}
	if k := (&OpenAIClient{}).cacheKeyFor(cachedReq(prefix, "x")); k != "" {
		t.Errorf("switch off but key %q", k)
	}
}

// Wire: the field is sent only when the switch is on AND the request marks a prefix;
// streaming sends the same key as a non-streaming call.
func TestPromptCacheKey_OnTheWire(t *testing.T) {
	t.Parallel()
	prefix := []domain.SystemBlock{{Text: "you are terse", Cache: true}, {Text: "rules"}}
	want := (&OpenAIClient{PromptCacheKey: true}).cacheKeyFor(cachedReq(prefix, ""))

	srv, cap := captureServer(t, openAIReply)
	off := &OpenAIClient{Endpoint: srv.URL + "/v1", Model: "m", TimeoutMs: 5000}
	if _, err := off.GenerateRequest(context.Background(), cachedReq(prefix, "hi")); err != nil {
		t.Fatal(err)
	}
	if _, _, body, _ := cap.get(); body["prompt_cache_key"] != nil {
		t.Fatalf("switch OFF (the default) sent prompt_cache_key=%v", body["prompt_cache_key"])
	}

	on := &OpenAIClient{Endpoint: srv.URL + "/v1", Model: "m", TimeoutMs: 5000, PromptCacheKey: true}
	resp, err := on.GenerateRequest(context.Background(), cachedReq(prefix, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, body, _ := cap.get(); body["prompt_cache_key"] != want {
		t.Fatalf("prompt_cache_key = %v, want %q", body["prompt_cache_key"], want)
	}
	// The per-block breakpoints themselves are still not honoured: reported as before.
	if !contains(resp.Ignored, domain.FieldCacheBreakpoints) {
		t.Errorf("Ignored = %v, want cache_breakpoints", resp.Ignored)
	}
	if _, err := on.GenerateRequest(context.Background(), cachedReq([]domain.SystemBlock{{Text: "plain"}}, "hi")); err != nil {
		t.Fatal(err)
	}
	if _, _, body, _ := cap.get(); body["prompt_cache_key"] != nil {
		t.Errorf("a request with no cache mark sent prompt_cache_key=%v", body["prompt_cache_key"])
	}

	sse := strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	ssrv, scap := captureServer(t, sse)
	son := &OpenAIClient{Endpoint: ssrv.URL + "/v1", Model: "m", TimeoutMs: 5000, PromptCacheKey: true}
	ch, err := son.StreamRequest(context.Background(), cachedReq(prefix, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)
	if _, _, body, _ := scap.get(); body["prompt_cache_key"] != want {
		t.Errorf("stream prompt_cache_key = %v, want %q", body["prompt_cache_key"], want)
	}
}

// End to end through the real configured path: a generators[] entry as an operator
// writes it in providers.json -> config validation -> NewProvider -> the generator the
// kernel hands out -> the OpenAI-compatible adapter -> a stub server that records the
// body. Two turns of one conversation share the key; a generator without the switch
// sends none.
func TestPromptCacheKey_E2E_ConfigToWire(t *testing.T) {
	t.Parallel()
	srvOn, capOn := captureServer(t, openAIReply)
	srvOff, capOff := captureServer(t, openAIReply)
	raw := `{"default":"cached","generators":[
	  {"id":"cached","provider":"openai","model":"m","endpoint":"` + srvOn.URL + `/v1","timeout_ms":5000,"prompt_cache_key":true},
	  {"id":"plain","provider":"openai","model":"m","endpoint":"` + srvOff.URL + `/v1","timeout_ms":5000}]}`
	full := config.DefaultConfig()
	if err := json.Unmarshal([]byte(raw), &full.LLMProvider); err != nil {
		t.Fatal(err)
	}
	if verr := full.Validate(); verr != nil && strings.Contains(verr.Error(), "llm_provider") {
		t.Fatalf("config refused the generators: %v", verr)
	}
	p, err := NewProvider(full.LLMProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	system := []domain.SystemBlock{{Text: "You are the assistant of Acme.", Cache: true}, {Text: "Time: 09:00"}}
	gen := p.GeneratorForModel("cached").(domain.RequestGenerator)
	var keys []any
	for _, q := range []string{"first question", "second question"} {
		if _, err := gen.GenerateRequest(context.Background(), cachedReq(system, q)); err != nil {
			t.Fatal(err)
		}
		_, _, body, _ := capOn.get()
		keys = append(keys, body["prompt_cache_key"])
	}
	if keys[0] == nil || keys[0] != keys[1] {
		t.Fatalf("keys across two turns = %v, want one shared non-empty key", keys)
	}
	if _, err := p.GeneratorForModel("plain").(domain.RequestGenerator).GenerateRequest(context.Background(), cachedReq(system, "q")); err != nil {
		t.Fatal(err)
	}
	if _, _, body, n := capOff.get(); n != 1 || body["prompt_cache_key"] != nil {
		t.Fatalf("plain generator: hits=%d prompt_cache_key=%v, want 1 hit and no key", n, body["prompt_cache_key"])
	}
}
