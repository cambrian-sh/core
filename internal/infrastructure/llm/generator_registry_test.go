package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
)

// fakeGen is a scriptable domain.Generator for decorator tests.
type fakeGen struct {
	out string
	err error
}

func (f fakeGen) Generate(_ context.Context, _ string) (string, error) { return f.out, f.err }

func TestGeneratorRegistry_DistinctOpenAIIDsCoexist(t *testing.T) {
	reg, err := NewGeneratorRegistry([]config.GeneratorConfig{
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://a/v1", APIKeyEnv: "K1"},
		{ID: "gpt4o", Provider: "openai", Model: "gpt-4o", Endpoint: "https://b/v1", APIKeyEnv: "K2"},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	a, ok1 := reg.Lookup("deepseek")
	b, ok2 := reg.Lookup("gpt4o")
	if !ok1 || !ok2 {
		t.Fatal("both ids must resolve (regression: provider-keyed last-wins)")
	}
	if a.Generator == b.Generator {
		t.Fatal("two openai generators must be distinct clients, not the same slot")
	}
}

func TestHealthGenerator_EmptyResponseRecordsFailure(t *testing.T) {
	b, _ := newTestBreaker(1, time.Minute) // threshold 1
	g := newHealthGenerator("x", fakeGen{out: "", err: nil}, b)
	_, _ = g.Generate(context.Background(), "p")
	if b.Healthy("x") {
		t.Fatal("empty response must record a failure and trip the breaker")
	}
}

func TestHealthGenerator_ErrorRecordsFailure(t *testing.T) {
	b, _ := newTestBreaker(1, time.Minute)
	g := newHealthGenerator("x", fakeGen{out: "", err: errors.New("boom")}, b)
	if _, err := g.Generate(context.Background(), "p"); err == nil {
		t.Fatal("error should propagate")
	}
	if b.Healthy("x") {
		t.Fatal("error must record a failure")
	}
}

func TestHealthGenerator_SuccessKeepsHealthy(t *testing.T) {
	b, _ := newTestBreaker(2, time.Minute)
	g := newHealthGenerator("x", fakeGen{out: "hello", err: nil}, b)
	if out, err := g.Generate(context.Background(), "p"); err != nil || out != "hello" {
		t.Fatalf("passthrough failed: %q %v", out, err)
	}
	if !b.Healthy("x") {
		t.Fatal("successful call must keep the id healthy")
	}
}

func TestOllamaClient_HTTPErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model 'qwen3:8b' not found"}`))
	}))
	defer srv.Close()

	c := &OllamaClient{BaseURL: srv.URL, Model: "qwen3:8b", TimeoutMs: 2000}
	out, err := c.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatalf("expected error on HTTP 404, got out=%q nil err (the silent-empty bug)", out)
	}
}

func TestOllamaClient_EmptyResponseReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":""}`)) // 200 but empty content
	}))
	defer srv.Close()

	c := &OllamaClient{BaseURL: srv.URL, Model: "qwen3:8b", TimeoutMs: 2000}
	if _, err := c.Generate(context.Background(), "hi"); err == nil {
		t.Fatal("expected error on empty response body, got nil")
	}
}
