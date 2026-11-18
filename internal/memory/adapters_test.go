package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func webhookDoc() domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI:  "https://ci.example.com/build/42",
		SourceType: "web",
		Title:      "Build 42 finished",
		Body:       "All tests passed.",
		Author:     "ci-bot",
		Timestamp:  time.Now(),
	}
}

func postDoc(t *testing.T, handler http.Handler, doc domain.ExternalDocument, token string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(doc)
	req := httptest.NewRequest("POST", "/v1/ingest", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Ingest-Token", token)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr.Result()
}

// Cycle 4 — valid token + valid JSON → 202 Accepted, document enqueued.
func TestWebhookReceiver_ValidRequest_Returns202(t *testing.T) {
	t.Parallel()
	var enqueued []domain.ExternalDocument
	enq := func(_ context.Context, doc domain.ExternalDocument) bool {
		enqueued = append(enqueued, doc)
		return true
	}
	handler := NewWebhookReceiver("secret-token", enq)

	resp := postDoc(t, handler, webhookDoc(), "secret-token")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
	if len(enqueued) != 1 {
		t.Errorf("expected 1 enqueued doc, got %d", len(enqueued))
	}
}

// Cycle 5 — wrong token → 401 Unauthorized.
func TestWebhookReceiver_WrongToken_Returns401(t *testing.T) {
	t.Parallel()
	handler := NewWebhookReceiver("secret-token", func(context.Context, domain.ExternalDocument) bool { return true })
	resp := postDoc(t, handler, webhookDoc(), "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// Cycle 6 — missing token → 401.
func TestWebhookReceiver_MissingToken_Returns401(t *testing.T) {
	t.Parallel()
	handler := NewWebhookReceiver("secret-token", func(context.Context, domain.ExternalDocument) bool { return true })
	resp := postDoc(t, handler, webhookDoc(), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// Cycle 7 — malformed JSON → 400 Bad Request.
func TestWebhookReceiver_MalformedJSON_Returns400(t *testing.T) {
	t.Parallel()
	handler := NewWebhookReceiver("tok", func(context.Context, domain.ExternalDocument) bool { return true })
	req := httptest.NewRequest("POST", "/v1/ingest", strings.NewReader("{invalid"))
	req.Header.Set("X-Ingest-Token", "tok")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// Cycle 8 — full queue → 503 Service Unavailable.
func TestWebhookReceiver_FullQueue_Returns503(t *testing.T) {
	t.Parallel()
	// Enqueue func returns false to simulate full queue.
	handler := NewWebhookReceiver("tok", func(context.Context, domain.ExternalDocument) bool { return false })
	resp := postDoc(t, handler, webhookDoc(), "tok")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

// ADR-0142 (K-3): an empty ingest token refuses every request; it no longer
// switches authentication off.
func TestWebhookReceiver_EmptyTokenFailsClosed(t *testing.T) {
	called := false
	h := NewWebhookReceiver("", func(context.Context, domain.ExternalDocument) bool { called = true; return true })
	for _, hdr := range []string{"", "anything"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(`{"body":"x"}`))
		if hdr != "" {
			req.Header.Set("X-Ingest-Token", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || called {
			t.Fatalf("token %q: status %d enqueued=%v", hdr, rec.Code, called)
		}
	}
	// Behind an authenticating wrapper the preauthenticated receiver enqueues.
	pre := NewPreauthenticatedWebhookReceiver(func(context.Context, domain.ExternalDocument) bool { called = true; return true })
	rec := httptest.NewRecorder()
	pre.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(`{"body":"x"}`)))
	if rec.Code != http.StatusAccepted || !called {
		t.Fatalf("preauthenticated: %d", rec.Code)
	}
}

// ENG-494: a token-authenticated push is written by the deterministic source
// principal source:webhook:ingest; the client cannot name the writer (Author is
// kernel-stamped, the client's value kept as AuthorLabel) or forge owner tags.
func TestWebhookReceiver_TokenPushRunsAsWebhookSourcePrincipal(t *testing.T) {
	t.Parallel()
	var gotP domain.PrincipalRef
	var got domain.ExternalDocument
	h := NewWebhookReceiver("tok", func(ctx context.Context, doc domain.ExternalDocument) bool {
		gotP, got = domain.PrincipalFromContext(ctx), doc
		return true
	})
	doc := webhookDoc()
	doc.Author = "agent-alice" // a forged writer of record
	doc.Tags = []string{"company_wide", domain.OwnerTag("bob")}
	if resp := postDoc(t, h, doc, "tok"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotP != WebhookIngestPrincipal || gotP.String() != "source:webhook:ingest" {
		t.Fatalf("principal %v, want source:webhook:ingest", gotP)
	}
	if got.Author != "source:webhook:ingest" || got.AuthorLabel != "agent-alice" {
		t.Fatalf("author %q label %q", got.Author, got.AuthorLabel)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "company_wide" {
		t.Fatalf("owner tag not dropped: %v", got.Tags)
	}
}

// ENG-494: behind the operator bearer the authenticated operator is the writer.
func TestWebhookReceiver_PreauthenticatedKeepsTheCaller(t *testing.T) {
	t.Parallel()
	var gotP domain.PrincipalRef
	var got domain.ExternalDocument
	h := NewPreauthenticatedWebhookReceiver(func(ctx context.Context, doc domain.ExternalDocument) bool {
		gotP, got = domain.PrincipalFromContext(ctx), doc
		return true
	})
	body, _ := json.Marshal(webhookDoc())
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(string(body)))
	req = req.WithContext(domain.WithPrincipal(req.Context(), domain.UserPrincipal("op-1")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if gotP != domain.UserPrincipal("op-1") || got.Author != "op-1" {
		t.Fatalf("principal %v author %q, want user:op-1 / op-1", gotP, got.Author)
	}
}
