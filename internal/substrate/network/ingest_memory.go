package network

import (
	"context"
	"errors"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MemoryWriter is the agent memory write-back surface (memory.remember()). The
// memory.RememberService satisfies it. Classification is kernel-derived (ADR-0035 C2).
type MemoryWriter interface {
	Remember(ctx context.Context, agentID, text string, hint []string, source, sessionID string, importance float64) (string, error)
}

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
		entityID, err := s.IngestionProcessor.ProcessSync(ctx, doc)
		if err != nil {
			return nil, status.Error(codes.Internal, "ingestion manager: "+err.Error())
		}
		return &pb.IngestMemoryResponse{DocId: entityID}, nil
	}
	if s.MemoryWriter == nil {
		return nil, status.Error(codes.Unimplemented, "memory write-back not configured")
	}
	agentID := callerAgentID(ctx)
	docID, err := s.MemoryWriter.Remember(ctx, agentID, req.GetText(), req.GetTags(), req.GetSource(), req.GetSessionId(), req.GetImportance())
	if err != nil {
		switch {
		case errors.Is(err, memory.ErrUnknownPrincipal):
			return nil, status.Error(codes.PermissionDenied, "unknown principal: "+agentID)
		case errors.Is(err, scope.ErrUnknownClassification):
			return nil, status.Error(codes.InvalidArgument, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &pb.IngestMemoryResponse{DocId: docID}, nil
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
