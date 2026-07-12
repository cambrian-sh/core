package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

type fakeIngestor struct {
	calls int
	last  operator.IngestRequest
	docID string
}

func (f *fakeIngestor) Ingest(_ context.Context, req operator.IngestRequest) (string, error) {
	f.calls++
	f.last = req
	return f.docID, nil
}

func newIngestService() (*operator.Service, *operator.InMemoryAuditStore, *fakeIngestor) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	audit := operator.NewInMemoryAuditStore()
	grants := domain.NewInMemoryGrantsStore()
	svc := operator.NewService(feed)
	svc.SetCommandSources(audit, grants)
	ing := &fakeIngestor{docID: "source_doc:op-1"}
	svc.SetMemoryIngestor(ing)
	return svc, audit, ing
}

// IngestMemory delegates to the kernel ingest path (operator as Author), records
// the doc_id, and a replayed command_id returns it without re-ingesting (A2.4).
func TestIngestMemory_IngestsAuditsAndDedups(t *testing.T) {
	svc, audit, ing := newIngestService()

	resp, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
		CommandId: "i1", Reason: "seed corpus", Text: "the payments team owns billing",
		Tags: []string{"eng"}, Importance: 0.7, Source: "handoff",
	})
	if err != nil {
		t.Fatalf("IngestMemory: %v", err)
	}
	if resp.GetDeduped() || resp.GetDocId() != "source_doc:op-1" {
		t.Fatalf("first ingest = %+v", resp)
	}
	if ing.calls != 1 || ing.last.Author != "alice" || ing.last.Text != "the payments team owns billing" {
		t.Fatalf("ingest request wrong: calls=%d last=%+v", ing.calls, ing.last)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "i1"})
	if len(rows) != 1 || rows[0].After != "source_doc:op-1" || rows[0].ActionType != "ingest_memory" {
		t.Fatalf("audit row = %+v", rows)
	}

	// Replay returns the original doc_id, no second ingest.
	replay, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
		CommandId: "i1", Reason: "seed corpus", Text: "the payments team owns billing",
	})
	if err != nil || !replay.GetDeduped() || replay.GetDocId() != "source_doc:op-1" {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	if ing.calls != 1 {
		t.Fatalf("replay must not re-ingest, calls=%d", ing.calls)
	}
}

func TestIngestMemory_ValidationAndUnconfigured(t *testing.T) {
	svc, _, _ := newIngestService()
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "x", Reason: "r"}); err == nil {
		t.Fatal("empty text should be InvalidArgument")
	}

	// No ingestor wired ⇒ Unimplemented.
	feed := operator.NewSpool(operator.SpoolConfig{})
	bare := operator.NewService(feed)
	bare.SetCommandSources(operator.NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	if _, err := bare.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "x", Reason: "r", Text: "hi"}); err == nil {
		t.Fatal("no ingestor should be Unimplemented")
	}
}
