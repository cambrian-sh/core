package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/memory"
)

type fakeDocLister struct {
	got   memory.DocumentFilter
	page  []memory.DocumentSummary
	next  string
	total int
	err   error
}

func (f *fakeDocLister) ListDocuments(_ context.Context, filter memory.DocumentFilter) ([]memory.DocumentSummary, string, int, error) {
	f.got = filter
	return f.page, f.next, f.total, f.err
}

// An unwired lister must say so rather than answering "no documents". The two are
// not the same claim: one means the kernel cannot enumerate, the other means the
// corpus is empty, and a console that cannot tell them apart hides a broken
// deployment behind a plausible empty state.
func TestListDocuments_UnwiredIsUnimplemented(t *testing.T) {
	svc, _, _, _ := newCommandService()

	_, err := svc.ListDocuments(context.Background(), &pb.ListDocumentsOpRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
	if svc.HasDocumentLister() {
		t.Fatal("HasDocumentLister should be false before wiring")
	}
}

func TestListDocuments_PassesFilterAndMapsRows(t *testing.T) {
	created := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	lister := &fakeDocLister{
		page: []memory.DocumentSummary{
			{ID: "doc-a", Title: "Ops review", SourceType: "pdf", Tags: nil, ChunkCount: 12, CreatedAt: created},
			{ID: "doc-b", Title: "Payroll", SourceType: "md", Tags: []string{"hr"}, ChunkCount: 3, CreatedAt: created},
		},
		next:  "doc-b",
		total: 422,
	}
	svc, _, _, _ := newCommandService()
	svc.SetDocumentLister(lister)

	resp, err := svc.ListDocuments(context.Background(), &pb.ListDocumentsOpRequest{
		Limit: 2, Cursor: "doc-0", UnlabelledOnly: true, IdPrefix: "doc-",
	})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}

	// The filter must reach the store intact — unlabelled_only is the whole point of
	// the RPC, and silently dropping it would return a plausible page of the wrong
	// documents.
	want := memory.DocumentFilter{Limit: 2, Cursor: "doc-0", UnlabelledOnly: true, IDPrefix: "doc-"}
	if lister.got != want {
		t.Fatalf("filter = %+v, want %+v", lister.got, want)
	}

	if len(resp.GetDocuments()) != 2 {
		t.Fatalf("want 2 rows, got %d", len(resp.GetDocuments()))
	}
	first := resp.GetDocuments()[0]
	if first.GetId() != "doc-a" || first.GetTitle() != "Ops review" || first.GetSourceType() != "pdf" {
		t.Fatalf("row 0 = %+v", first)
	}
	if first.GetChunkCount() != 12 {
		t.Fatalf("chunk_count = %d, want 12", first.GetChunkCount())
	}
	if first.GetCreatedAtUnixMs() != created.UnixMilli() {
		t.Fatalf("created_at = %d, want %d", first.GetCreatedAtUnixMs(), created.UnixMilli())
	}
	if len(first.GetTags()) != 0 {
		t.Fatalf("doc-a should be unlabelled, got %v", first.GetTags())
	}

	// The total describes the whole matching set, not the page. "422 of 1163" is what
	// tells an operator how much of the corpus no rule can reach; "2 shown" does not.
	if resp.GetTotalMatching() != 422 {
		t.Fatalf("total_matching = %d, want 422", resp.GetTotalMatching())
	}
	if resp.GetNextCursor() != "doc-b" {
		t.Fatalf("next_cursor = %q", resp.GetNextCursor())
	}
}

func TestListDocuments_StoreErrorSurfaces(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetDocumentLister(&fakeDocLister{err: errors.New("connection refused")})

	_, err := svc.ListDocuments(context.Background(), &pb.ListDocumentsOpRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}
