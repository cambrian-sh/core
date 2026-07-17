package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// IngestRequest is the operator's document-ingest intent (ADR-0047 A2.4). Author
// is the operator principal; the kernel derives classification and stamps
// provenance — tags are a narrow-only hint, never a raw write.
type IngestRequest struct {
	Text       string
	Tags       []string
	Importance float64
	Source     string
	SessionID  string
	Author     string
	// Content is the raw bytes of a binary upload (PDF/DOCX/...). Mutually exclusive
	// with Text. Routed to the ADR-0060 structure parser's Docling backend.
	Content []byte
	// Filename is the original name; its extension drives chunker routing. Required
	// when Content is set.
	Filename string
	// ContentType is an advisory MIME hint; the extension wins for routing.
	ContentType string
	// Context is the operator's note about this document, folded into the body at
	// ingest so it is chunked and embedded with the content.
	Context string
}

// MemoryIngestor requests a kernel document ingest and returns the assigned
// doc_id. Wired to the kernel IngestionProcessor.ProcessSync (chunking pipeline)
// / MemoryWriter.Remember — never a raw store write (A2.4). nil ⇒ Unimplemented.
type MemoryIngestor interface {
	Ingest(ctx context.Context, req IngestRequest) (docID string, err error)
}

// MemoryIngestorFunc adapts a plain function to MemoryIngestor so the composition
// root binds the kernel ingest path without the operator package importing it.
type MemoryIngestorFunc func(ctx context.Context, req IngestRequest) (string, error)

func (f MemoryIngestorFunc) Ingest(ctx context.Context, req IngestRequest) (string, error) {
	return f(ctx, req)
}

// SetMemoryIngestor wires the operator IngestMemory path (A2.4). nil ⇒ Unimplemented.
func (s *Service) SetMemoryIngestor(ing MemoryIngestor) { s.ingestor = ing }

// IngestMemory requests a kernel document ingest on the operator's authority
// (A2.4). The operator principal is stamped as Author; the kernel derives
// classification (tags are a narrow-only hint). Idempotent on command_id: a
// retry returns the original doc_id from the audit row without re-ingesting
// (which would duplicate chunks and skew retrieval).
func (s *Service) IngestMemory(ctx context.Context, req *pb.IngestMemoryOpRequest) (*pb.IngestMemoryOpResponse, error) {
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	// Exactly one body lane: text OR bytes. Both set is ambiguous (which one is the
	// document?); neither is empty. Bytes with no filename cannot be chunker-routed,
	// so the extension is mandatory on the binary lane.
	hasText, hasContent := req.GetText() != "", len(req.GetContent()) > 0
	switch {
	case !hasText && !hasContent:
		return nil, status.Error(codes.InvalidArgument, "one of text or content is required")
	case hasText && hasContent:
		return nil, status.Error(codes.InvalidArgument, "text and content are mutually exclusive")
	case hasContent && req.GetFilename() == "":
		return nil, status.Error(codes.InvalidArgument, "filename is required when content is set")
	}
	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	if s.ingestor == nil {
		return nil, status.Error(codes.Unimplemented, "operator memory ingest not configured")
	}

	// Idempotency: a replayed command_id returns the original doc_id, no re-ingest.
	prior, err := s.audit.Query(ctx, AuditFilter{CommandID: req.GetCommandId(), Limit: 1})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit lookup: %v", err)
	}
	if len(prior) == 1 {
		return &pb.IngestMemoryOpResponse{CommandId: req.GetCommandId(), Deduped: true, DocId: prior[0].After}, nil
	}

	actor, role, _ := PrincipalFromContext(ctx)
	docID, err := s.ingestor.Ingest(ctx, IngestRequest{
		Text:        req.GetText(),
		Tags:        req.GetTags(),
		Importance:  req.GetImportance(),
		Source:      req.GetSource(),
		SessionID:   req.GetSessionId(),
		Author:      actor,
		Content:     req.GetContent(),
		Filename:    req.GetFilename(),
		ContentType: req.GetContentType(),
		Context:     req.GetContext(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ingest memory: %v", err)
	}

	entry := domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "ingest_memory",
		TargetType: "document", TargetID: docID,
		After: docID, Reason: req.GetReason(), Result: "ok",
	}
	if _, err := s.recordAndEmit(ctx, entry); err != nil {
		return nil, err
	}
	return &pb.IngestMemoryOpResponse{CommandId: req.GetCommandId(), Deduped: false, DocId: docID}, nil
}
