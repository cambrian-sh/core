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
	SourceURI   string
	SourceType  string // "slack", "email", "web", "jira", "pdf", "file_drop"
	Title       string
	Body        string
	Author      string
	Timestamp   time.Time
	ThreadID    string
	Attachments []Attachment
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
