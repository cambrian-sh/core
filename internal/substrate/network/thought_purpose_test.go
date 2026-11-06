package network

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// T-20261106-thought-steps-call-the-model-under-the-planner (live E2E E31): a
// kernel thought step is a deliverable. Its model call reaches post_model hooks
// labelled "thought", so a redaction scoped [agent_step, thought] rewrites the
// answer while one scoped [agent_step] — or [planner] — does not see it.

type thoughtRedactHook struct {
	name     string
	purposes []string
	calls    int
}

func (h *thoughtRedactHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: h.name, Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true, Purposes: h.purposes}
}

func (h *thoughtRedactHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	h.calls++
	s := strings.ReplaceAll(ev.Text, "CODEWORD-42", "[REDACTED]")
	return domain.HookResult{Text: &s}, nil
}

func thoughtStepAnswer(t *testing.T, h *thoughtRedactHook) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop",`+
			`"message":{"role":"assistant","content":"the codeword is CODEWORD-42"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	chain, err := domain.NewHookChain([]domain.Hook{h}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := llm.NewProvider(config.LLMProviderConfig{
		Default:    "g",
		Generators: []config.GeneratorConfig{{ID: "g", Provider: "openai", Model: "m", Endpoint: srv.URL, TimeoutMs: 5000}},
		Roles:      map[string]string{"planner": "g"},
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, llm.WithHooks(chain))
	if err != nil {
		t.Fatal(err)
	}
	// The composition root's wiring (internal/kernel/provider.go).
	s := &Server{Planner: &mockPlanner{}, ThoughtLLM: p.GeneratorFor(domain.PurposeThought)}
	resp, err := s.thoughtFn(thoughtPlan())(t.Context(), 0, thoughtHandoff(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	return string(resp.Payload.Data)
}

func TestThoughtStep_ModelCallIsLabelledThoughtForHooks(t *testing.T) {
	for _, tc := range []struct {
		purposes []string
		redacted bool
	}{
		{[]string{"agent_step", "thought"}, true},
		{[]string{"thought"}, true},
		{[]string{"agent_step"}, false},
		{[]string{"planner"}, false},
	} {
		h := &thoughtRedactHook{name: "e2e-redact-answer", purposes: tc.purposes}
		got := thoughtStepAnswer(t, h)
		if redacted := !strings.Contains(got, "CODEWORD-42"); redacted != tc.redacted {
			t.Errorf("purposes %v: answer %q, redacted=%v, want %v", tc.purposes, got, redacted, tc.redacted)
		}
		if seen := h.calls > 0; seen != tc.redacted {
			t.Errorf("purposes %v: hook calls = %d", tc.purposes, h.calls)
		}
	}
}
