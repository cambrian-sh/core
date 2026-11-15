package memory

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cambrian-sh/core/domain"
)

// WebhookIngestPrincipal is the source service principal of documents pushed to
// /v1/ingest with the shared ingest token (ENG-494).
var WebhookIngestPrincipal = domain.SourcePrincipal("webhook", "ingest")

// WebhookReceiver is a net/http.Handler for POST /v1/ingest (ADR-0028).
// It validates the X-Ingest-Token header and enqueues accepted documents.
type WebhookReceiver struct {
	token   string
	enqueue func(context.Context, domain.ExternalDocument) bool
	// preauthenticated is set only by NewPreauthenticatedWebhookReceiver: the
	// caller was already authenticated by a wrapping handler (the operator bearer).
	preauthenticated bool
}

// NewWebhookReceiver constructs a WebhookReceiver checking X-Ingest-Token.
// enqueue must be non-nil.
//
// An EMPTY token now refuses every request (ADR-0142 trust holes, feasibility
// K-3). It used to disable the check — "useful for local dev" — which made
// /v1/ingest an anonymous write into memory with no principal at all. A
// deployment without an ingest token authenticates pushers with the operator
// bearer instead (NewPreauthenticatedWebhookReceiver behind requireOperatorBearer).
func NewWebhookReceiver(token string, enqueue func(context.Context, domain.ExternalDocument) bool) *WebhookReceiver {
	return &WebhookReceiver{token: token, enqueue: enqueue}
}

// NewPreauthenticatedWebhookReceiver builds a receiver that performs NO token
// check because the handler wrapping it already authenticated the caller. It is
// only correct behind such a wrapper; app.go mounts it behind the operator bearer.
func NewPreauthenticatedWebhookReceiver(enqueue func(context.Context, domain.ExternalDocument) bool) *WebhookReceiver {
	return &WebhookReceiver{enqueue: enqueue, preauthenticated: true}
}

// ServeHTTP handles POST /v1/ingest requests.
func (wr *WebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Constant-time compare (2026-10-31 audit). Fail closed: an empty configured
	// token matches nothing (ADR-0142, K-3); see NewWebhookReceiver.
	if !wr.preauthenticated && (wr.token == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Ingest-Token")), []byte(wr.token)) != 1) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var doc domain.ExternalDocument
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ENG-494 (owner decision 2026-11-15): the pushed document is written by a real
	// principal. Behind the operator bearer it is the authenticated operator the
	// wrapper put on ctx; with the shared ingest token — one credential, one source —
	// it is the deterministic source service principal source:webhook:ingest (an
	// operator can give it a write ceiling like any source). Either way the writer
	// of record is kernel-stamped: Author was decodable from the JSON body, so a
	// pusher could name any writer; what it sent is kept as the descriptive
	// AuthorLabel. Owner tags are kernel-only (ADR-0146 D5) and dropped here, at the
	// caller-controlled boundary.
	ctx := r.Context()
	p := domain.PrincipalFromContext(ctx)
	if !wr.preauthenticated || p.IsZero() {
		p = WebhookIngestPrincipal
		ctx = domain.WithPrincipal(ctx, p)
	}
	if doc.AuthorLabel == "" {
		doc.AuthorLabel = doc.Author
	}
	doc.Author = p.WriterID()
	doc.Tags = domain.DropCallerOwnerTags(ctx, "v1_ingest.tags", doc.Tags)

	if !wr.enqueue(ctx, doc) {
		slog.Warn("WebhookReceiver: ingestion queue full, rejecting request", "source_uri", doc.SourceURI)
		http.Error(w, "service unavailable: ingestion queue full", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"queued"}`))
}
