package domain

import (
	"context"
	"time"
)

// IngestionAdapter is the port for external knowledge sources (ADR-0028).
// Adapters may poll on a schedule (Poll) or stream events in real-time (Stream).
type IngestionAdapter interface {
	Name() string
	Poll(ctx context.Context, since time.Time) ([]ExternalDocument, error)
	Stream(ctx context.Context) (<-chan ExternalDocument, error)
}

// ExternalDocument is a unit of external knowledge ready for ingestion.
type ExternalDocument struct {
	SourceURI  string
	SourceType string // "slack", "email", "web", "jira", "pdf", "file_drop"
	Title      string
	Body       string
	// Data carries the ORIGINAL bytes for binary documents (PDF, DOCX, ...) so the
	// ADR-0060 structure parser can hand them to the docling_agent's Docling backend
	// instead of a flattened text approximation. Empty for text documents, which
	// travel in Body. When set, Body may be empty — the parser's leaves become the
	// chunk set.
	Data []byte
	// Author is the KERNEL-STAMPED writer: the authenticated principal's id on the agent
	// plane. It becomes the chunks' source_agent_id — the identity read scope, erasure
	// and the ADR-0137 writer-of-record rely on. A client never sets it (AuthorLabel is
	// the client's descriptive author).
	Author string
	// SourceKind is the CLIENT's free-text description of the source ("conversation",
	// "email", "report", ...; ADR-0136 A3.1). Descriptive data only: SourceType stays
	// kernel-owned because parser routing reads it, and nothing branches on SourceKind.
	// The fact extractor shows it as the source type when set (SourceMetaOf).
	SourceKind string
	// AuthorLabel is the source's author as the source names it (A3.2). Descriptive
	// only: it never replaces Author.
	AuthorLabel string
	// Timestamp is the document's EVENT time — when the content happened
	// (ADR-0134), in UTC. An ingest with no stated time sets it to the ingest
	// clock and TimeSource to TimeSourceIngestDefault, so the two are never
	// confused. The fields after it qualify it; ResolveIngestEventTime fills them.
	Timestamp   time.Time
	ThreadID    string
	Attachments []Attachment
	Tags        []string
	// Labels are the writer's non-classification markers (ENG-2170; domain.MetaLabels):
	// what the document IS (a revision's lifecycle, "rev3", corpus/document ids),
	// never who may read it. Already normalised (NormalizeLabels) by the door that
	// accepted them. The write chokepoints never read or filter them; they are stored
	// on the document row and on every chunk.
	Labels     []string
	Importance float64

	// EventTimeEnd is the span end of a multi-part document (latest part time);
	// zero for a single instant.
	EventTimeEnd time.Time
	// TimeSource says where Timestamp came from; empty reads as ingest_default.
	TimeSource TimeSource
	// TimePrecision is how precise Timestamp is; empty reads as second.
	TimePrecision TimePrecision
	// ClaimedTime is the caller's stated time when it was clamped
	// (TimeSource == TimeSourceClampedFuture); zero otherwise.
	ClaimedTime time.Time
	// UploadName is the file name of an OPERATOR UPLOAD (set only by the operator
	// door, from the uploaded filename). It makes the upload a named source: the
	// document is keyed by (writer, classification scope, normalised name), so a
	// changed re-upload is a new version of it and an identical one is a no-op
	// (ADR-0138 Amendment B1). Empty for every other ingest.
	UploadName string
	// Parts locates each timed part of a multi-part body (a chat session's turns)
	// inside Body, so a chunk can take the earliest time of the parts it covers and
	// record the latest as its span end (ADR-0134 Q5). Empty for a single-part body.
	Parts []DocumentPart
}

// ApplyEventTime copies a resolved event time onto the document.
func (d *ExternalDocument) ApplyEventTime(et IngestEventTime) {
	d.Timestamp = et.EventTime
	d.EventTimeEnd = et.EventTimeEnd
	d.TimeSource = et.Source
	d.TimePrecision = et.Precision
	d.ClaimedTime = et.ClaimedTime
	d.Parts = et.Parts
}

// Attachment is a binary or text file associated with an ExternalDocument.
type Attachment struct {
	Name     string
	MIMEType string
	Body     string
}

// ExternalDocumentIngested is emitted by IngestionManager after a document batch
// is committed to the Tier-2 pipeline (ADR-0028). Carries provenance for audit.
type ExternalDocumentIngested struct {
	SourceURI  string
	ChunkCount int
}

func (ExternalDocumentIngested) domainEvent() {}
