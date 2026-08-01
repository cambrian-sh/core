package memory

import (
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
	var enqueued []domain.ExternalDocument
	enq := func(doc domain.ExternalDocument) bool {
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
	handler := NewWebhookReceiver("secret-token", func(_ domain.ExternalDocument) bool { return true })
	resp := postDoc(t, handler, webhookDoc(), "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// Cycle 6 — missing token → 401.
func TestWebhookReceiver_MissingToken_Returns401(t *testing.T) {
	handler := NewWebhookReceiver("secret-token", func(_ domain.ExternalDocument) bool { return true })
	resp := postDoc(t, handler, webhookDoc(), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// Cycle 7 — malformed JSON → 400 Bad Request.
func TestWebhookReceiver_MalformedJSON_Returns400(t *testing.T) {
	handler := NewWebhookReceiver("tok", func(_ domain.ExternalDocument) bool { return true })
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
	// Enqueue func returns false to simulate full queue.
	handler := NewWebhookReceiver("tok", func(_ domain.ExternalDocument) bool { return false })
	resp := postDoc(t, handler, webhookDoc(), "tok")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}
