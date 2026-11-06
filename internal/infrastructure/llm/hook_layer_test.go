package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0139 D11 pre_model / post_model on the Provider request path.

type modelHook struct {
	spec domain.HookSpec
	run  func(ev domain.HookEvent) domain.HookResult
}

func (h modelHook) Spec() domain.HookSpec { return h.spec }
func (h modelHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	return h.run(ev), nil
}

// bodyServer answers every call with content and records each request body.
type bodyServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newBodyServer(t *testing.T, content string) *bodyServer {
	t.Helper()
	bs := &bodyServer{}
	bs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bs.mu.Lock()
		bs.bodies = append(bs.bodies, string(b))
		bs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody(content, "stop"))
	}))
	t.Cleanup(bs.srv.Close)
	return bs
}

func (bs *bodyServer) last() string {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if len(bs.bodies) == 0 {
		return ""
	}
	return bs.bodies[len(bs.bodies)-1]
}

func hookedProvider(t *testing.T, bs *bodyServer, hs ...domain.Hook) *Provider {
	t.Helper()
	chain, err := domain.NewHookChain(hs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(config.LLMProviderConfig{
		Default:    "a",
		Generators: []config.GeneratorConfig{{ID: "a", Provider: "openai", Model: "m", Endpoint: bs.srv.URL, TimeoutMs: 5000}},
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, WithHooks(chain))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rewriteMessages() domain.Hook {
	return modelHook{domain.HookSpec{Name: "rw", Points: []domain.HookPoint{domain.HookPreModel}, MayRewrite: true},
		func(ev domain.HookEvent) domain.HookResult {
			msgs := append([]domain.ModelMessage(nil), ev.Model.Messages...)
			msgs[0].Content = strings.ReplaceAll(msgs[0].Content, "SSN 123", "SSN [x]")
			return domain.HookResult{Messages: msgs, Annotations: []string{"answer briefly"}}
		}}
}

func redactReply() domain.Hook {
	return modelHook{domain.HookSpec{Name: "red", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true},
		func(ev domain.HookEvent) domain.HookResult {
			s := strings.ReplaceAll(ev.Text, "secret", "[R]")
			return domain.HookResult{Text: &s}
		}}
}

func TestModelHooks_RewriteAndRedactOnTheC1Path(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, `"the secret is out"`)
	p := hookedProvider(t, bs, rewriteMessages(), redactReply())

	resp, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).
		GenerateRequest(context.Background(), domain.UserPrompt("my SSN 123"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "the [R] is out" {
		t.Errorf("post_model rewrite: %q", resp.Text)
	}
	body := bs.last()
	if strings.Contains(body, "SSN 123") || !strings.Contains(body, "SSN [x]") || !strings.Contains(body, "rw: answer briefly") {
		t.Errorf("pre_model rewrite/annotation did not reach the wire: %s", body)
	}
}

func TestModelHooks_LegacyPromptAndAgentChain(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, `"a secret"`)
	p := hookedProvider(t, bs, rewriteMessages(), redactReply())

	out, err := p.GeneratorFor(domain.PurposeMemory).Generate(context.Background(), "SSN 123")
	if err != nil || out != "a [R]" || strings.Contains(bs.last(), "SSN 123") {
		t.Fatalf("legacy Generate: %q %v body=%s", out, err, bs.last())
	}
	resp, _, err := p.GenerateOn(context.Background(), []string{"a"}, domain.UserPrompt("SSN 123"))
	if err != nil || resp.Text != "a [R]" || strings.Contains(bs.last(), "SSN 123") {
		t.Fatalf("GenerateOn: %q %v body=%s", resp.Text, err, bs.last())
	}
}

func TestModelHooks_DenyAndAskRefuseWithoutAWireCall(t *testing.T) {
	t.Parallel()
	for _, v := range []domain.HookVerdict{domain.HookDeny, domain.HookAsk} {
		bs := newBodyServer(t, `"x"`)
		p := hookedProvider(t, bs, modelHook{domain.HookSpec{Name: "g", Points: []domain.HookPoint{domain.HookPreModel}},
			func(domain.HookEvent) domain.HookResult { return domain.HookResult{Verdict: v, Reason: "blocked"} }})
		_, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).
			GenerateRequest(context.Background(), domain.UserPrompt("hi"))
		if !errors.Is(err, domain.ErrHookDenied) || bs.last() != "" {
			t.Errorf("%s: err=%v wire=%q", v, err, bs.last())
		}
		if _, _, err := p.StreamOn(context.Background(), []string{"a"}, domain.UserPrompt("hi")); !errors.Is(err, domain.ErrHookDenied) {
			t.Errorf("%s stream: err=%v", v, err)
		}
	}
}

func TestModelHooks_PostModelDenyWithholdsTheReply(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, `"leak"`)
	p := hookedProvider(t, bs, modelHook{domain.HookSpec{Name: "w", Points: []domain.HookPoint{domain.HookPostModel}},
		func(ev domain.HookEvent) domain.HookResult {
			if ev.Text == "leak" {
				return domain.HookResult{Verdict: domain.HookDeny, Reason: "pii"}
			}
			return domain.HookResult{}
		}})
	out, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "hi")
	if !errors.Is(err, domain.ErrHookDenied) || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
