package memory

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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

	// ENG-2110: the body is decoded STRICTLY. A field domain.ExternalDocument does not
	// have is the pusher's error (a typo'd "body" used to decode to an empty document,
	// answer 202 and store an empty row), refused with the field named — the same
	// rule as the kernel's other caller-facing JSON (hooks, specs, subtasks). The body
	// size is capped by the listener's limitBody (http.MaxBytesReader); hitting it is
	// 413, not a generic 400.
	var doc domain.ExternalDocument
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("request body too large: the limit is %d bytes", tooLarge.Limit),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// ENG-2110: a push with no usable text and no file bytes is refused now, while
	// the pusher is still listening — queued, it could only be dropped later. (Bytes
	// are checked after the parser reads them, on the queue; see processBatchDoc.)
	if len(doc.Data) == 0 && !domain.HasUsableText(domain.StorableText(doc.Body)) {
		why := "the body is empty"
		if doc.Body != "" {
			why = "the body is whitespace only"
		}
		http.Error(w, fmt.Sprintf("bad request: %v from %s: %s", domain.ErrNoUsableText, doc.IngestDisplayName(), why),
			http.StatusBadRequest)
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
