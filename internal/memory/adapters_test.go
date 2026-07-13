package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── DirectoryWatcher tests ───────────────────────────────────────────────────

// Cycle 1 — Poll returns files newer than 'since' with correct ExternalDocument fields.
func TestDirectoryWatcher_Poll_ReturnsNewFiles(t *testing.T) {
	dir := t.TempDir()
	watcher := NewDirectoryWatcher(dir, nil)

	before := time.Now().Add(-time.Second)
	content := "# Hello\n\nThis is a markdown file."
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	docs, err := watcher.Poll(context.Background(), before)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}
	doc := docs[0]
	if doc.SourceType != "md" {
		t.Errorf("SourceType: want %q got %q", "md", doc.SourceType)
	}
	if doc.Body != content {
		t.Errorf("Body mismatch")
	}
	if !strings.HasSuffix(doc.SourceURI, "note.md") {
		t.Errorf("SourceURI: got %q", doc.SourceURI)
	}
}

// Cycle 2 — Poll ignores files with unsupported extensions.
func TestDirectoryWatcher_Poll_IgnoresUnsupportedExtensions(t *testing.T) {
	dir := t.TempDir()
	watcher := NewDirectoryWatcher(dir, nil)

	_ = os.WriteFile(filepath.Join(dir, "binary.exe"), []byte("data"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "data.csv"), []byte("a,b"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "valid.txt"), []byte("hello"), 0644)

	docs, err := watcher.Poll(context.Background(), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc (valid.txt only), got %d", len(docs))
	}
	if docs[0].SourceType != "txt" {
		t.Errorf("unexpected SourceType: %q", docs[0].SourceType)
	}
}

// Cycle 3 — Poll ignores files older than 'since'.
func TestDirectoryWatcher_Poll_IgnoresOldFiles(t *testing.T) {
	dir := t.TempDir()
	watcher := NewDirectoryWatcher(dir, nil)

	// Write a file, then set Poll's since to a future time.
	_ = os.WriteFile(filepath.Join(dir, "old.txt"), []byte("old"), 0644)
	future := time.Now().Add(time.Hour)

	docs, err := watcher.Poll(context.Background(), future)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected 0 docs for future since, got %d", len(docs))
	}
}

// ── WebhookReceiver tests ────────────────────────────────────────────────────

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
