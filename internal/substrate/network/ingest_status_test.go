package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeIngestStatus map[string]domain.IngestStatus

func (f fakeIngestStatus) IngestStatus(_ context.Context, id string) (domain.IngestStatus, bool, error) {
	st, ok := f[id]
	return st, ok, nil
}

// ENG-2143: the agent-plane status read answers only the document's writer; another
// caller is told NOT_FOUND exactly as for an unknown id.
func TestGetIngestStatus_WriterOnly(t *testing.T) {
	s := &Server{IngestStatus: fakeIngestStatus{"doc-1": {
		DocumentID: "doc-1", State: domain.IngestEmbedding, ChunksTotal: 10, ChunksEmbedded: 4, Writer: "alice",
	}}}
	as := func(agent string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", agent))
	}
	got, err := s.GetIngestStatus(as("alice"), &pb.GetIngestStatusRequest{DocumentId: "doc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != "embedding" || got.GetChunksTotal() != 10 || got.GetChunksEmbedded() != 4 || got.GetDocumentId() != "doc-1" {
		t.Fatalf("status = %+v", got)
	}
	for _, tc := range []struct {
		ctx  context.Context
		id   string
		code codes.Code
	}{
		{as("mallory"), "doc-1", codes.NotFound},
		{as("alice"), "doc-2", codes.NotFound},
		{context.Background(), "doc-1", codes.PermissionDenied},
		{as("alice"), "", codes.InvalidArgument},
	} {
		if _, err := s.GetIngestStatus(tc.ctx, &pb.GetIngestStatusRequest{DocumentId: tc.id}); status.Code(err) != tc.code {
			t.Errorf("GetIngestStatus(%q) = %v, want %v", tc.id, err, tc.code)
		}
	}
	if _, err := (&Server{}).GetIngestStatus(as("alice"), &pb.GetIngestStatusRequest{DocumentId: "doc-1"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("unwired = %v, want Unimplemented", err)
	}
}

// acceptingProcessor reports an accepted large document through the ingest outcome, as
// memory.IngestionManager does for a background embed.
type acceptingProcessor struct{}

func (acceptingProcessor) ProcessSync(ctx context.Context, _ domain.ExternalDocument) (string, error) {
	if out := domain.IngestOutcomeFrom(ctx); out != nil {
		out.DocumentID = "doc-big"
		out.Status = domain.IngestStatus{DocumentID: "doc-big", State: domain.IngestAccepted, ChunksTotal: 5295}
	}
	return "source_doc:big", nil
}

// ENG-2143: IngestMemory returns the document id and its state with the entity id.
func TestIngestMemory_ResponseCarriesDocumentAndStatus(t *testing.T) {
	s := &Server{IngestionProcessor: acceptingProcessor{}}
	resp, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{Text: "big body"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDocId() != "source_doc:big" || resp.GetDocumentId() != "doc-big" ||
		resp.GetStatus().GetState() != "accepted" || resp.GetStatus().GetChunksTotal() != 5295 {
		t.Fatalf("response = %+v", resp)
	}
}
