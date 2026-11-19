package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeIngestStatus map[string]domain.IngestStatus

func (f fakeIngestStatus) IngestStatus(_ context.Context, id string) (domain.IngestStatus, bool, error) {
	st, ok := f[id]
	return st, ok, nil
}

// acceptingIngestor reports an accepted large upload through the ingest outcome, as the
// kernel ingest does for a background embed.
type acceptingIngestor struct{}

func (acceptingIngestor) Ingest(ctx context.Context, _ operator.IngestRequest) (string, error) {
	if out := domain.IngestOutcomeFrom(ctx); out != nil {
		out.DocumentID, out.Version = "doc-big", 1
		out.Status = domain.IngestStatus{DocumentID: "doc-big", State: domain.IngestAccepted, ChunksTotal: 900}
	}
	return "source_doc:big", nil
}

// ENG-2143: the operator plane reports an upload's state on IngestMemory and reads its
// progress with GetIngestStatus.
func TestIngestStatus_OperatorPlane(t *testing.T) {
	statuses := fakeIngestStatus{"doc-big": {DocumentID: "doc-big", State: domain.IngestEmbedding, ChunksTotal: 900, ChunksEmbedded: 256}}
	svc := operatortest.New(t, operator.NewSpool(operator.SpoolConfig{}), func(d *operator.Deps, o *operator.Options) {
		d.Ingestor = acceptingIngestor{}
		o.IngestStatus = statuses
	})
	resp, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "big-1", Reason: "upload", Text: "a large body"})
	if err != nil {
		t.Fatal(err)
	}
	if st := resp.GetStatus(); st.GetState() != "accepted" || st.GetChunksTotal() != 900 || st.GetDocumentId() != "doc-big" {
		t.Fatalf("IngestMemory status = %+v", st)
	}
	got, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "doc-big"})
	if err != nil || !got.GetFound() || got.GetStatus().GetState() != "embedding" || got.GetStatus().GetChunksEmbedded() != 256 {
		t.Fatalf("GetIngestStatus = %+v, %v", got, err)
	}
	if got, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "nope"}); err != nil || got.GetFound() {
		t.Fatalf("unknown id = %+v, %v; want found=false", got, err)
	}
	if _, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id = %v", err)
	}
	bare := operatortest.New(t, operator.NewSpool(operator.SpoolConfig{}), nil)
	if _, err := bare.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "doc-big"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("unwired = %v, want Unimplemented", err)
	}
}
