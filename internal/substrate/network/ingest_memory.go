package network

import (
	"context"
	"errors"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MemoryWriter is the agent memory write-back surface (memory.remember()). The
// memory.RememberService satisfies it. Classification is kernel-derived (ADR-0035 C2).
type MemoryWriter interface {
	Remember(ctx context.Context, agentID, text string, hint []string, source, sessionID string, importance float64) (string, error)
}

// IngestMemory commits agent-synthesized knowledge to LTM. The kernel DERIVES the
// document's classification from the agent's DefaultWriteTags (req.tags is a
// narrow-only hint); provenance is kernel-stamped. The agent cannot broaden.
// ADR-0035 (C2) / REQ-SDK-005b.
func (s *Server) IngestMemory(ctx context.Context, req *pb.IngestMemoryRequest) (*pb.IngestMemoryResponse, error) {
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
