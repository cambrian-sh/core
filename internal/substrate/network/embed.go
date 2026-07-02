package network

import (
	"context"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Embed embeds text into a vector using the kernel's embedder (ADR-0041). An
// agent's Local Recurrent Workspace calls this to relevance-rank its episodic
// buffer. It is read-only and reads no scoped store, so it carries no
// authorization impact — the single minimal kernel surface LRW adds.
func (s *Server) Embed(ctx context.Context, req *pb.EmbedRequest) (*pb.EmbedResponse, error) {
	if s.Embedder == nil {
		return nil, status.Error(codes.Unimplemented, "Embed: embedder not configured")
	}
	vec, err := s.Embedder.Embed(ctx, req.GetText())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Embed: %v", err)
	}
	return &pb.EmbedResponse{Vector: vec}, nil
}
