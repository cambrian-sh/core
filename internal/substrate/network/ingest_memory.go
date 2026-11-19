package network

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/llmusage"

	"github.com/cambrian-sh/core/internal/memory"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// IngestTimingTrailer is the gRPC trailer key carrying a sync ingest's per-stage
// timing (memory.FormatStageClock). Diagnostic, not a contract: keys may change.
const IngestTimingTrailer = "cambrian-ingest-timing"

// IngestionProcessor is the chunking-pipeline entry point. The gRPC
// IngestMemory handler routes through this when the Server has it
// wired (non-nil). Satisfied by *memory.IngestionManager. Contract:
// the implementation chunks the body, mints a source-doc entity,
// ingests each chunk with chunk_relations populated, and returns
// the source-doc entity ID.
//
// The interface is declared in this file (alongside MemoryWriter)
// because IngestMemory is the only caller; the gRPC handler
// otherwise wouldn't need to import the memory package directly.
type IngestionProcessor interface {
	ProcessSync(ctx context.Context, doc domain.ExternalDocument) (string, error)
}

// IngestMemory commits agent-synthesized knowledge to LTM. The kernel DERIVES the
// document's classification from the agent's DefaultWriteTags (req.tags is a
// narrow-only hint); provenance is kernel-stamped. The agent cannot broaden.
// ADR-0035 (C2) / REQ-SDK-005b.
//
// When the Server has an IngestionProcessor wired (the chunking-pipeline path,
// ADR-0060 D8/D9), the call routes through it: the body becomes a single
// ExternalDocument, the chunker registry splits it, a source-doc entity gets
// minted, and each chunk lands in LTM with chunk_relations.parent_entity_id
// set. The returned DocId is the source-doc entity ID (e.g. "source_doc:<uri>"),
// not a per-item fact ID. Falls back to MemoryWriter when IngestionProcessor
// is nil (legacy path).
func (s *Server) IngestMemory(ctx context.Context, req *pb.IngestMemoryRequest) (*pb.IngestMemoryResponse, error) {
	// ADR-0142 D12: knowledge an agent commits while working a person's
	// conversation is private to that person — the owner is resolved from the
	// presented lease (never the request) and every chunk, document and fact the
	// write chokepoints classify under ctx carries owner:<person>.
	ctx = s.withConversationOwner(ctx)
	// Fail closed on an unknown principal, BEFORE any work.
	//
	// This check lived only in RememberService — on the raw-write fallback that
	// could never fire — so the live chunked path had no equivalent: an agent with
	// no scope profile reached the enforcing store, which only asks ClassifyWrite
	// and has no notion of "we have never heard of you". Moved here so it guards
	// the path that actually runs.
	//
	// A nil read predicate is the decision point saying it could not resolve the
	// principal at all, which is the difference between "registered but unprofiled"
	// (unrestricted) and "unknown" (deny).
	if s.Authz != nil {
		agentID := callerAgentID(ctx)
		if agentID == "" {
			return nil, status.Error(codes.PermissionDenied, "unknown principal: no agent identity on the call")
		}
		if pred, _ := s.Authz.ReadFilter(ctx, domain.AgentPrincipal(agentID), domain.SurfaceFromContext(ctx)); pred == nil {
			return nil, status.Error(codes.PermissionDenied, "unknown principal: "+agentID)
		}
	}

	// ADR-0134 D1 (contract 0099): resolve the event time BEFORE any work, so a
	// malformed time is the caller's INVALID_ARGUMENT and never a half-done ingest.
	et, err := s.resolveIngestEventTime(req)
	if err != nil {
		return nil, err
	}

	// ADR-0136 A3.1: the client's descriptive source metadata, validated BEFORE any work
	// (a control character is the caller's INVALID_ARGUMENT, never a half-done ingest).
	meta, err := s.resolveIngestSourceMeta(req)
	if err != nil {
		return nil, err
	}

	if s.IngestionProcessor != nil {
		sourceURI := req.GetSource()
		if sourceURI == "" {
			sourceURI = "ingest_memory://" + req.GetSessionId()
		}
		body := req.GetText()
		if len(req.GetParts()) > 0 {
			body = et.Body
		}
		title := meta.title
		if title == "" {
			title = firstLine(body, 80)
		}
		doc := domain.ExternalDocument{
			SourceURI: sourceURI,
			// SourceType stays KERNEL-OWNED (A3.2): parser routing reads it, so a
			// client's source_kind goes to SourceKind and can never steer it.
			SourceType: "ingest_memory",
			SourceKind: meta.kind,
			Title:      title,
			Body:       body,
			// Author is the authenticated caller — the writer of record that becomes
			// source_agent_id (scope, erasure, ADR-0137). The client's author is
			// descriptive only and goes to AuthorLabel; it can never replace this.
			Author:      callerAgentID(ctx),
			AuthorLabel: meta.author,
			ThreadID:    req.GetSessionId(),
			// ADR-0146 D5: owner tags are kernel-stamped (withConversationOwner above),
			// never taken from the request.
			Tags:       append([]string(nil), domain.DropCallerOwnerTags(ctx, "IngestMemory.tags", req.GetTags())...),
			Importance: float64(req.GetImportance()),
		}
		doc.ApplyEventTime(et)
		// Stamp the authenticated principal, exactly as the Remember path below does.
		// Without it every write this ingest performs reaches the chokepoint with no
		// identity: OSS fails open and never notices, while a premium deployment fails
		// CLOSED and rejects the whole ingest with `no_principal`. That asymmetry is why
		// it survived — the path is only broken where the check actually works.
		ictx := ctx
		if agentID := callerAgentID(ctx); agentID != "" {
			ictx = domain.WithPrincipal(ctx, domain.AgentPrincipal(agentID))
		}
		// The per-stage breakdown of this ingest goes back to the caller as the
		// "cambrian-ingest-timing" trailer ("stage=ms,...,count#=n"), so a harness
		// can attribute ingest cost without parsing kernel logs and without a proto
		// change (T-20261102-ingestion-logs-no-per-stage-timing-so-the-1-s).
		clock := domain.NewStageClock()
		ictx = domain.WithStageClock(ictx, clock)
		// The kernel's own LLM calls for this ingest (fact extraction, assessment, any
		// organ the pipeline reaches) go back as the "cambrian-llm-usage" trailer,
		// counted by the ADR-0131 SpendMeter (llmusage.Trailer).
		ictx, sendLLMUsage := llmusage.WithTally(ictx)
		// ENG-2143: the ingest reports the stored document and where it stands (accepted
		// for a large document embedded in the background).
		outcome := &domain.IngestOutcome{}
		ictx = domain.WithIngestOutcome(ictx, outcome)
		started := time.Now()
		entityID, err := s.IngestionProcessor.ProcessSync(ictx, doc)
		_ = grpc.SetTrailer(ctx, metadata.Pairs(IngestTimingTrailer, memory.FormatStageClock(clock, time.Since(started)))) // best-effort diagnostic; fails only outside a gRPC call (tests)
		sendLLMUsage()
		if err != nil {
			// Map the CAUSE, not the layer. Collapsing everything to Internal is how a
			// denied write or a coined tag surfaced as
			// "ingestion manager: failed to mint source-doc entity [INTERNAL]" — a
			// message that names the symptom, hides the reason, and sent more than one
			// debugging session after the wrong thing entirely.
			switch {
			case errors.Is(err, authz.ErrWriteDenied):
				return nil, status.Error(codes.InvalidArgument, err.Error())
			case errors.Is(err, domain.ErrNoUsableText):
				// ENG-2110: content that yields no usable text is refused, nothing
				// stored; the message names the document and why.
				return nil, status.Error(codes.InvalidArgument, err.Error())
			case errors.Is(err, domain.ErrDocumentOwnedByAnotherWriter):
				// ADR-0138 Q1: another writer holds this caller-named document id. Refused,
				// never taken over — the earlier writer keeps read access to its content.
				return nil, status.Error(codes.PermissionDenied, err.Error())
			case errors.Is(err, memory.ErrFactsNotReady):
				// ENG-488: sync_ingest_wait_for_facts ran out of time. The content IS
				// stored; DeadlineExceeded tells the caller the read-your-writes wait,
				// not the ingest, failed — and a retry is an idempotent no-op that
				// waits again.
				return nil, status.Error(codes.DeadlineExceeded, err.Error())
			default:
				return nil, status.Error(codes.Internal, "ingestion manager: "+err.Error())
			}
		}
		documentID := outcome.DocumentID
		if outcome.DuplicateOf != "" {
			documentID = outcome.DuplicateOf
		}
		resp := &pb.IngestMemoryResponse{DocId: entityID, DocumentId: documentID, Status: ingestStatusPB(outcome.Status)}
		if resp.Status == nil {
			resp.Status = s.readIngestStatus(ctx, documentID)
		}
		return resp, nil
	}
	// EVERYTHING ingested goes through the chunker. There is no second way in.
	//
	// This used to fall back to a direct MemoryWriter.Remember — a raw store write
	// that produced a STRUCTURALLY DIFFERENT row: one un-chunked blob with a single
	// embedding, no source-document entity, no chunk_relations, no structural
	// sections, invisible to ListDocuments, and carrying "session_id" where the
	// chunked path deliberately writes an ingest-thread id instead.
	//
	// It was also unreachable: NewIngestionManager never returns nil (a zero queue
	// size defaults to 1000), so the fallback could not fire in any real deployment
	// — while still reading, to anyone auditing this file, as a live second path
	// with different semantics. Dead code that changes the answer to "how is memory
	// written" is worse than no code.
	//
	// If the pipeline is genuinely absent the ingest FAILS rather than silently
	// degrading the shape of everything it writes (the ADR-0060 fail-loud rule:
	// an unknown route is an error, not a quiet fallback).
	return nil, status.Error(codes.FailedPrecondition,
		"ingestion pipeline not configured: every memory ingest must go through the "+
			"chunker, and there is no raw-store-write path")
}

// resolveIngestEventTime validates the ADR-0134 event-time fields of an agent-plane
// ingest and resolves them through domain.ResolveIngestEventTime — the one
// implementation the operator plane shares — with the configured future-time
// tolerance (execution.ingestion.event_time_future_tolerance_ms).
func (s *Server) resolveIngestEventTime(req *pb.IngestMemoryRequest) (domain.IngestEventTime, error) {
	if len(req.GetParts()) > 0 && req.GetText() != "" {
		return domain.IngestEventTime{}, status.Error(codes.InvalidArgument, "text and parts are mutually exclusive")
	}
	occurredAt, err := wireTime("occurred_at", req.GetOccurredAt())
	if err != nil {
		return domain.IngestEventTime{}, err
	}
	caps := s.ExecCfg.Ingestion.SourceMetaCaps()
	parts := make([]domain.MemoryPartInput, 0, len(req.GetParts()))
	for i, p := range req.GetParts() {
		t, err := wireTime(fmt.Sprintf("parts[%d].occurred_at", i), p.GetOccurredAt())
		if err != nil {
			return domain.IngestEventTime{}, err
		}
		speaker, err := descriptiveField(fmt.Sprintf("parts[%d].speaker", i), p.GetSpeaker(), caps.Speaker)
		if err != nil {
			return domain.IngestEventTime{}, err
		}
		parts = append(parts, domain.MemoryPartInput{Text: p.GetText(), OccurredAt: t, Speaker: speaker})
	}
	tolerance := time.Duration(s.ExecCfg.Ingestion.EventTimeFutureToleranceMs) * time.Millisecond
	et, err := domain.ResolveIngestEventTime(occurredAt, parts, req.GetTimePrecision(), time.Now(), tolerance)
	if err != nil {
		return domain.IngestEventTime{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return et, nil
}

// ingestSourceMeta is the client's validated descriptive source metadata (A3.1).
type ingestSourceMeta struct {
	kind, title, author string
}

// resolveIngestSourceMeta validates source_kind, title and author against the
// configured caps (execution.ingestion.source_*_max_chars).
func (s *Server) resolveIngestSourceMeta(req *pb.IngestMemoryRequest) (ingestSourceMeta, error) {
	caps := s.ExecCfg.Ingestion.SourceMetaCaps()
	var m ingestSourceMeta
	var err error
	if m.kind, err = descriptiveField("source_kind", req.GetSourceKind(), caps.Kind); err != nil {
		return m, err
	}
	if m.title, err = descriptiveField("title", req.GetTitle(), caps.Title); err != nil {
		return m, err
	}
	if m.author, err = descriptiveField("author", req.GetAuthor(), caps.Author); err != nil {
		return m, err
	}
	return m, nil
}

// descriptiveField normalises one free-text descriptive value (ADR-0136 A3.1) through
// the one rule both planes share (domain.NormalizeDescriptiveField); a refusal is
// INVALID_ARGUMENT naming the field.
func descriptiveField(field, v string, max int) (string, error) {
	out, err := domain.NormalizeDescriptiveField(v, max)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%s: %v", field, err)
	}
	return out, nil
}

// wireTime converts an optional wire Timestamp. A Timestamp is an absolute UTC
// instant, so there is no zone to reject here; what can be wrong is the value
// itself (outside years 1-9999, bad nanos), and that is the caller's error.
func wireTime(field string, ts *timestamppb.Timestamp) (*time.Time, error) {
	if ts == nil {
		return nil, nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s: %v", field, err)
	}
	t := ts.AsTime().UTC()
	return &t, nil
}

func firstLine(s string, max int) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			if i > max {
				return s[:max]
			}
			return s[:i]
		}
	}
	if len(s) > max {
		return s[:max]
	}
	return s
}

// IngestStatusReader reads where an ingested document stands (ENG-2143). Satisfied by
// *memory.IngestionManager.
type IngestStatusReader interface {
	IngestStatus(ctx context.Context, documentID string) (domain.IngestStatus, bool, error)
}

// GetIngestStatus implements the ENG-2143 read on the agent plane: the progress of a
// document accepted for background embedding (accepted → embedding n/total →
// searchable, or failed with a reason). Only the document's writer of record may read
// it: any other caller gets NOT_FOUND, exactly as for an unknown id, so the read
// discloses nothing about another principal's documents (fail closed on a caller
// with no agent identity).
func (s *Server) GetIngestStatus(ctx context.Context, req *pb.GetIngestStatusRequest) (*pb.IngestStatus, error) {
	if s.IngestStatus == nil {
		return nil, status.Error(codes.Unimplemented, "ingest status not configured")
	}
	if req.GetDocumentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "document_id is required")
	}
	caller := callerAgentID(ctx)
	if caller == "" {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: no agent identity on the call")
	}
	st, found, err := s.IngestStatus.IngestStatus(ctx, req.GetDocumentId())
	if err != nil {
		return nil, status.Error(codes.Internal, "ingest status: "+err.Error())
	}
	if !found || st.Writer != caller {
		return nil, status.Error(codes.NotFound, "no such document of this caller: "+req.GetDocumentId())
	}
	return ingestStatusPB(st), nil
}

// readIngestStatus is the status for an IngestMemory response the ingest did not
// report (a duplicate upload); nil when unreadable.
func (s *Server) readIngestStatus(ctx context.Context, documentID string) *pb.IngestStatus {
	if s.IngestStatus == nil || documentID == "" {
		return nil
	}
	st, found, err := s.IngestStatus.IngestStatus(ctx, documentID)
	if err != nil || !found {
		return nil
	}
	return ingestStatusPB(st)
}

func ingestStatusPB(st domain.IngestStatus) *pb.IngestStatus {
	if st.State == "" {
		return nil
	}
	return &pb.IngestStatus{
		DocumentId: st.DocumentID, State: string(st.State),
		ChunksTotal: int32(st.ChunksTotal), ChunksEmbedded: int32(st.ChunksEmbedded), Error: st.Error,
	}
}
