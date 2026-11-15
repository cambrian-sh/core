package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fixedFactStats struct{ st memory.FactExtractorStats }

func (f fixedFactStats) Stats() memory.FactExtractorStats { return f.st }

// ENG-431: the Snapshot carries the fact extractor's counters when extraction runs,
// and omits the field when it does not.
func TestSnapshot_ReportsFactExtractorStats(t *testing.T) {
	t.Parallel()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil || resp.GetFactExtractor() != nil {
		t.Fatalf("no extractor wired: fact_extractor = %v err %v, want absent", resp.GetFactExtractor(), err)
	}
	svc.SetFactStats(fixedFactStats{memory.FactExtractorStats{
		Enqueued: 9, Dropped: 2, FailedDocs: 1, PartialDocs: 3, Stored: 40, RefusedQuoteNotFound: 5, RefusedOverCap: 4,
	}})
	resp, err = svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fe := resp.GetFactExtractor()
	if fe.GetEnqueued() != 9 || fe.GetDroppedQueueFull() != 2 || fe.GetFailedDocuments() != 1 || fe.GetPartialDocuments() != 3 ||
		fe.GetStored() != 40 || fe.GetRefusedQuoteNotFound() != 5 || fe.GetRefusedOverCap() != 4 {
		t.Fatalf("fact_extractor = %+v", fe)
	}
}
