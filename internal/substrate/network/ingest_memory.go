package network

import (
	"context"
	"errors"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

	if s.IngestionProcessor != nil {
		sourceURI := req.GetSource()
		if sourceURI == "" {
			sourceURI = "ingest_memory://" + req.GetSessionId()
		}
		doc := domain.ExternalDocument{
			SourceURI:  sourceURI,
			SourceType: "ingest_memory",
			Title:      firstLine(req.GetText(), 80),
			Body:       req.GetText(),
			Author:     callerAgentID(ctx),
			Timestamp:  time.Now().UTC(),
			ThreadID:   req.GetSessionId(),
			Tags:       append([]string(nil), req.GetTags()...),
			Importance: float64(req.GetImportance()),
		}
		// Stamp the authenticated principal, exactly as the Remember path below does.
		// Without it every write this ingest performs reaches the chokepoint with no
		// identity: OSS fails open and never notices, while a premium deployment fails
		// CLOSED and rejects the whole ingest with `no_principal`. That asymmetry is why
		// it survived — the path is only broken where the check actually works.
		ictx := ctx
		if agentID := callerAgentID(ctx); agentID != "" {
			ictx = domain.WithPrincipal(ctx, domain.AgentPrincipal(agentID))
		}
		entityID, err := s.IngestionProcessor.ProcessSync(ictx, doc)
		if err != nil {
			// Map the CAUSE, not the layer. Collapsing everything to Internal is how a
			// denied write or a coined tag surfaced as
			// "ingestion manager: failed to mint source-doc entity [INTERNAL]" — a
			// message that names the symptom, hides the reason, and sent more than one
			// debugging session after the wrong thing entirely.
			switch {
			case errors.Is(err, authz.ErrWriteDenied):
				return nil, status.Error(codes.InvalidArgument, err.Error())
			default:
				return nil, status.Error(codes.Internal, "ingestion manager: "+err.Error())
			}
		}
		return &pb.IngestMemoryResponse{DocId: entityID}, nil
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
