package operator

import (
	"context"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// IngestStatusReader reads where an ingested document stands (ENG-2143): a large
// upload is accepted at once and embedded in the background, and the console shows
// its progress. Satisfied by *memory.IngestionManager.
type IngestStatusReader interface {
	IngestStatus(ctx context.Context, documentID string) (domain.IngestStatus, bool, error)
}

// GetIngestStatus implements the ENG-2143 read: accepted → embedding n/total →
// searchable, or failed with a reason. Read RPC; found=false for an unknown id. The
// operator plane reads documents at system scope (ADR-0047 D13), so no writer check.
func (s *Service) GetIngestStatus(ctx context.Context, req *pb.GetIngestStatusOpRequest) (*pb.GetIngestStatusOpResponse, error) {
	if s.ingestStatus == nil {
		return nil, status.Error(codes.Unimplemented, "ingest status not configured")
	}
	if req.GetDocumentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "document_id is required")
	}
	st, found, err := s.ingestStatus.IngestStatus(ctx, req.GetDocumentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ingest status: %v", err)
	}
	if !found {
		return &pb.GetIngestStatusOpResponse{Found: false}, nil
	}
	return &pb.GetIngestStatusOpResponse{Found: true, Status: ingestStatusOp(st)}, nil
}

// ingestStatusOf reads one document's status for a response field; nil when it cannot
// be read (no reader, unknown id, a store error — the field is informative).
func (s *Service) ingestStatusOf(ctx context.Context, documentID string) *pb.IngestStatusOp {
	if s.ingestStatus == nil || documentID == "" {
		return nil
	}
	st, found, err := s.ingestStatus.IngestStatus(ctx, documentID)
	if err != nil || !found {
		return nil
	}
	return ingestStatusOp(st)
}

func ingestStatusOp(st domain.IngestStatus) *pb.IngestStatusOp {
	if st.State == "" {
		return nil
	}
	return &pb.IngestStatusOp{
		DocumentId: st.DocumentID, State: string(st.State),
		ChunksTotal: int32(st.ChunksTotal), ChunksEmbedded: int32(st.ChunksEmbedded), Error: st.Error,
	}
}
