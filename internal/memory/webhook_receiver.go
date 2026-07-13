package memory

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cambrian-sh/core/domain"
)

// WebhookReceiver is a net/http.Handler for POST /v1/ingest (ADR-0028).
// It validates the X-Ingest-Token header and enqueues accepted documents.
type WebhookReceiver struct {
	token   string
	enqueue func(domain.ExternalDocument) bool
}

// NewWebhookReceiver constructs a WebhookReceiver. enqueue must be non-nil.
// An empty token string disables token validation (useful for local dev/tests).
func NewWebhookReceiver(token string, enqueue func(domain.ExternalDocument) bool) *WebhookReceiver {
	return &WebhookReceiver{token: token, enqueue: enqueue}
}

// ServeHTTP handles POST /v1/ingest requests.
func (wr *WebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if wr.token != "" && r.Header.Get("X-Ingest-Token") != wr.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var doc domain.ExternalDocument
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !wr.enqueue(doc) {
		slog.Warn("WebhookReceiver: ingestion queue full, rejecting request", "source_uri", doc.SourceURI)
		http.Error(w, "service unavailable: ingestion queue full", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"queued"}`))
}
