package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// outcomeIngestor fills the ADR-0138 B1 ingest outcome the way the ingestion manager
// does, through ctx.
type outcomeIngestor struct{ out domain.IngestOutcome }

func (o *outcomeIngestor) Ingest(ctx context.Context, _ operator.IngestRequest) (string, error) {
	if rep := domain.IngestOutcomeFrom(ctx); rep != nil {
		*rep = o.out
	}
	return o.out.DocumentID, nil
}

// Contract 0100: an exact duplicate upload says "duplicate of <id>" and a replacement
// says which version it became.
func TestIngestMemory_ReportsDuplicateAndVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  domain.IngestOutcome
	}{
		{"duplicate", domain.IngestOutcome{DocumentID: "operator_ingest://r.pdf#w=1", DuplicateOf: "operator_ingest://r.pdf#w=1", Version: 2}},
		{"replaced", domain.IngestOutcome{DocumentID: "operator_ingest://r.pdf#w=1", Version: 3, Replaced: true}},
	} {
		svc, _, _ := newIngestService()
		svc.SetMemoryIngestor(&outcomeIngestor{out: tc.out})
		resp, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
			CommandId: "c-" + tc.name, Reason: "upload", Content: []byte("%PDF"), Filename: "r.pdf",
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.GetDuplicateOf() != tc.out.DuplicateOf || resp.GetVersion() != int32(tc.out.Version) || resp.GetDeduped() {
			t.Errorf("%s: response %+v", tc.name, resp)
		}
	}
}

// Contract 0100: ListDocuments with upload_name is the replace-by-name lookup. The
// writer is the authenticated operator, never a request field; a match is one row.
func TestListDocuments_UploadNameLookup(t *testing.T) {
	t.Parallel()
	svc, _, _ := newIngestService()
	if _, err := svc.ListDocuments(opCtx(), &pb.ListDocumentsOpRequest{UploadName: "r.pdf"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("unwired lookup: want Unimplemented, got %v", err)
	}
	if svc.HasUploadMatcher() {
		t.Fatal("HasUploadMatcher before wiring")
	}
	updated := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	var got operator.IngestRequest
	svc.SetUploadMatcher(operator.UploadMatcherFunc(func(_ context.Context, req operator.IngestRequest) (domain.DocumentRecord, error) {
		got = req
		if req.Filename != "Report.pdf" {
			return domain.DocumentRecord{}, nil
		}
		return domain.DocumentRecord{ID: "operator_ingest://report.pdf#w=1", Exists: true, Title: "report.pdf",
			Version: 2, CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated}, nil
	}))
	resp, err := svc.ListDocuments(opCtx(), &pb.ListDocumentsOpRequest{UploadName: "Report.pdf", UploadTags: []string{"finance"}})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Author != "alice" || got.Filename != "Report.pdf" || len(got.Tags) != 1 || got.Tags[0] != "finance" {
		t.Errorf("matcher request: %+v", got)
	}
	if len(resp.GetDocuments()) != 1 || resp.GetTotalMatching() != 1 {
		t.Fatalf("want one row, got %+v", resp)
	}
	row := resp.GetDocuments()[0]
	if row.GetId() != "operator_ingest://report.pdf#w=1" || row.GetVersion() != 2 || row.GetUpdatedAtUnixMs() != updated.UnixMilli() {
		t.Errorf("row %+v", row)
	}
	none, err := svc.ListDocuments(opCtx(), &pb.ListDocumentsOpRequest{UploadName: "other.pdf"})
	if err != nil || len(none.GetDocuments()) != 0 {
		t.Errorf("no match: %+v, %v", none, err)
	}
}
