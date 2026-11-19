package operator

import (
	"context"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"

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
//
// ENG-2184 (owner decision 2026-11-19): document_id may also be a source-doc ENTITY
// id ("source_doc:<uri>" — what IngestMemory returns and every MemoryWrittenOp
// carries). It is resolved to its document rows by the document listing's own lookup
// (the one identity rule, postgres ListDocuments), under the listing's read scope (the
// owner fence), and the answer is the status of the MOST RECENTLY WRITTEN readable
// row — status.document_id names it. Several writers of one source are several rows;
// a row the caller may not read is never the answer, and none readable is
// found=false.
func (s *Service) GetIngestStatus(ctx context.Context, req *pb.GetIngestStatusOpRequest) (*pb.GetIngestStatusOpResponse, error) {
	if s.ingestStatus == nil {
		return nil, status.Error(codes.Unimplemented, "ingest status not configured")
	}
	if req.GetDocumentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "document_id is required")
	}
	docID := req.GetDocumentId()
	if domain.IsSourceDocEntityID(docID) {
		row, ok, err := s.resolveEntityRow(ctx, docID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "ingest status: %v", err)
		}
		if !ok {
			return &pb.GetIngestStatusOpResponse{Found: false}, nil
		}
		docID = row
	}
	st, found, err := s.ingestStatus.IngestStatus(ctx, docID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ingest status: %v", err)
	}
	if !found {
		return &pb.GetIngestStatusOpResponse{Found: false}, nil
	}
	return &pb.GetIngestStatusOpResponse{Found: true, Status: ingestStatusOp(st)}, nil
}

// resolveEntityRow is the document row a source-doc entity id answers for: the most
// recently written row the listing lets this reader see (ENG-2184). false when the
// kernel has no document listing or no readable row carries the entity.
func (s *Service) resolveEntityRow(ctx context.Context, entityID string) (string, bool, error) {
	if s.documents == nil {
		return "", false, nil
	}
	rows, _, _, err := s.documents.ListDocuments(ctx, memory.DocumentFilter{IDPrefix: entityID, Limit: 1})
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	return rows[0].ID, true, nil
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
