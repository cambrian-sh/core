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

// fakeLister answers an entity-id lookup with the rows it was given (newest first, as
// the Postgres listing orders them) and records the filter it was asked with.
type fakeLister struct {
	rows map[string][]domain.DocumentSummary
	got  []domain.DocumentFilter
}

func (f *fakeLister) ListDocuments(_ context.Context, flt domain.DocumentFilter) ([]domain.DocumentSummary, string, int, error) {
	f.got = append(f.got, flt)
	r := f.rows[flt.IDPrefix]
	if flt.Limit > 0 && len(r) > flt.Limit {
		r = r[:flt.Limit]
	}
	return r, "", len(f.rows[flt.IDPrefix]), nil
}

// ENG-2184 (owner decision 2026-11-19): GetIngestStatus accepts the source-doc entity
// id the console holds and answers with the status of the most recently written row
// the listing lets the reader see; none readable → found=false.
func TestIngestStatus_ResolvesASourceDocEntityID(t *testing.T) {
	statuses := fakeIngestStatus{
		"report#w=bbb": {DocumentID: "report#w=bbb", State: domain.IngestEmbedding, ChunksTotal: 300, ChunksEmbedded: 40},
		"report#w=aaa": {DocumentID: "report#w=aaa", State: domain.IngestSearchable},
	}
	lister := &fakeLister{rows: map[string][]domain.DocumentSummary{
		// Two writers of one source: the newest row first.
		"source_doc:report": {{ID: "report#w=bbb"}, {ID: "report#w=aaa"}},
	}}
	svc := operatortest.New(t, operator.NewSpool(operator.SpoolConfig{}), func(_ *operator.Deps, o *operator.Options) {
		o.IngestStatus = statuses
		o.Documents = lister
	})
	got, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "source_doc:report"})
	if err != nil || !got.GetFound() {
		t.Fatalf("entity id = %+v, %v; want found", got, err)
	}
	if st := got.GetStatus(); st.GetDocumentId() != "report#w=bbb" || st.GetState() != "embedding" || st.GetChunksEmbedded() != 40 {
		t.Fatalf("status = %+v, want the newest row's (report#w=bbb, embedding 40/300)", st)
	}
	if f := lister.got[len(lister.got)-1]; f.IDPrefix != "source_doc:report" || f.Limit != 1 || f.Scope != nil {
		t.Fatalf("the resolution must use the listing's own lookup and scope: %+v", f)
	}
	// An entity no readable row carries.
	if got, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "source_doc:other"}); err != nil || got.GetFound() {
		t.Fatalf("unresolved entity = %+v, %v; want found=false", got, err)
	}
	// A row id is read as before, never through the listing.
	n := len(lister.got)
	if got, err := svc.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "report#w=aaa"}); err != nil || got.GetStatus().GetState() != "searchable" {
		t.Fatalf("row id = %+v, %v", got, err)
	}
	if len(lister.got) != n {
		t.Fatal("a row id must not go through the entity resolution")
	}
	// No document listing: an entity id cannot be resolved.
	bare := operatortest.New(t, operator.NewSpool(operator.SpoolConfig{}), func(_ *operator.Deps, o *operator.Options) {
		o.IngestStatus = statuses
	})
	if got, err := bare.GetIngestStatus(opCtx(), &pb.GetIngestStatusOpRequest{DocumentId: "source_doc:report"}); err != nil || got.GetFound() {
		t.Fatalf("no lister = %+v, %v; want found=false", got, err)
	}
}
