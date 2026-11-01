package operator

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	// EventTime is the resolved ADR-0134 event time (contract 0099): when the
	// content happened, where that came from, its precision, and — for a
	// multi-part upload — the assembled body (EventTime.Body, which replaces Text)
	// and the part offsets. Always set by IngestMemory; an upload that states no
	// time carries the ingest clock labelled ingest_default.
	EventTime domain.IngestEventTime
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

// SetEventTimeFutureTolerance sets how far past the ingest clock a stated event
// time may lie before it is clamped (ADR-0134 Q4; config
// execution.ingestion.event_time_future_tolerance_ms). Unset means
// domain.DefaultEventTimeFutureTolerance.
func (s *Service) SetEventTimeFutureTolerance(d time.Duration) { s.eventTimeTolerance = &d }

func (s *Service) eventTimeFutureTolerance() time.Duration {
	if s.eventTimeTolerance == nil {
		return domain.DefaultEventTimeFutureTolerance
	}
	return *s.eventTimeTolerance
}

// resolveOpEventTime validates the ADR-0134 event-time fields of an operator
// ingest and resolves them through domain.ResolveIngestEventTime, the one
// implementation both planes share.
func (s *Service) resolveOpEventTime(req *pb.IngestMemoryOpRequest) (domain.IngestEventTime, error) {
	occurredAt, err := opWireTime("occurred_at", req.GetOccurredAt())
	if err != nil {
		return domain.IngestEventTime{}, err
	}
	parts := make([]domain.MemoryPartInput, 0, len(req.GetParts()))
	for i, p := range req.GetParts() {
		t, err := opWireTime(fmt.Sprintf("parts[%d].occurred_at", i), p.GetOccurredAt())
		if err != nil {
			return domain.IngestEventTime{}, err
		}
		parts = append(parts, domain.MemoryPartInput{Text: p.GetText(), OccurredAt: t})
	}
	et, err := domain.ResolveIngestEventTime(occurredAt, parts, req.GetTimePrecision(), time.Now(), s.eventTimeFutureTolerance())
	if err != nil {
		return domain.IngestEventTime{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return et, nil
}

// opWireTime converts an optional wire Timestamp (an absolute UTC instant, so no
// zone can be missing); an out-of-range value is the caller's error.
func opWireTime(field string, ts *timestamppb.Timestamp) (*time.Time, error) {
	if ts == nil {
		return nil, nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s: %v", field, err)
	}
	t := ts.AsTime().UTC()
	return &t, nil
}

// IngestMemory requests a kernel document ingest on the operator's authority
// (A2.4). The operator principal is stamped as Author; the kernel derives
// classification (tags are a narrow-only hint). Idempotent on command_id: a
// retry returns the original doc_id from the audit row without re-ingesting
// (which would duplicate chunks and skew retrieval).
func (s *Service) IngestMemory(ctx context.Context, req *pb.IngestMemoryOpRequest) (*pb.IngestMemoryOpResponse, error) {
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	// Exactly one body lane: text, bytes, or timed parts (ADR-0134, contract 0099).
	// More than one is ambiguous (which one is the document?); none is empty. Bytes
	// with no filename cannot be chunker-routed, so the extension is mandatory on
	// the binary lane.
	hasText, hasContent, hasParts := req.GetText() != "", len(req.GetContent()) > 0, len(req.GetParts()) > 0
	lanes := 0
	for _, b := range []bool{hasText, hasContent, hasParts} {
		if b {
			lanes++
		}
	}
	switch {
	case lanes == 0:
		return nil, status.Error(codes.InvalidArgument, "one of text, content or parts is required")
	case lanes > 1:
		return nil, status.Error(codes.InvalidArgument, "text, content and parts are mutually exclusive")
	case hasContent && req.GetFilename() == "":
		return nil, status.Error(codes.InvalidArgument, "filename is required when content is set")
	}
	et, err := s.resolveOpEventTime(req)
	if err != nil {
		return nil, err
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

	// No principal stamping here. The interceptor sets the domain principal for
	// every operator-plane RPC (see authorize), so ctx already carries the
	// authenticated operator. This handler used to do it by hand, which worked
	// and was a workaround: the next handler that wrote anything had to remember
	// to grow the same four lines, and the one that forgot would fail closed only
	// on a deployment that enforces access control.
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
		EventTime:   et,
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
