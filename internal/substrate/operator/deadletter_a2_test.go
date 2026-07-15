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

type fakeDeadLetterReader struct {
	entries []domain.ReactiveDeadLetter
}

func (f *fakeDeadLetterReader) ListDeadLetters(limit int) ([]domain.ReactiveDeadLetter, error) {
	out := append([]domain.ReactiveDeadLetter(nil), f.entries...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// REACT-01 / ADR-0061: without a reader wired (OSS build) the RPC is Unimplemented.
func TestDeadLetters_UnimplementedWhenUnwired(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	_, err := svc.ListWatchDeadLetters(context.Background(), &pb.ListWatchDeadLettersOpRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v", err)
	}
}

// With a reader wired, entries are mapped to the proto shape.
func TestDeadLetters_ReturnsMappedEntries(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	svc.SetDeadLetterReader(&fakeDeadLetterReader{entries: []domain.ReactiveDeadLetter{
		{
			ID: "dl-1", WatchID: "w1", ActionType: "start_plan", Key: "k1", Reason: "boom",
			Signal:   domain.Signal{StreamID: "s1", RawText: "hello"},
			FailedAt: time.Unix(0, 1_000_000*1234).UTC(),
		},
	}})
	resp, err := svc.ListWatchDeadLetters(context.Background(), &pb.ListWatchDeadLettersOpRequest{})
	if err != nil {
		t.Fatalf("ListWatchDeadLetters: %v", err)
	}
	if len(resp.GetEntries()) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.GetEntries()))
	}
	e := resp.GetEntries()[0]
	if e.GetId() != "dl-1" || e.GetWatchId() != "w1" || e.GetActionType() != "start_plan" ||
		e.GetReason() != "boom" || e.GetSignalStreamId() != "s1" || e.GetSignalRawText() != "hello" {
		t.Fatalf("unexpected mapping: %+v", e)
	}
	if e.GetFailedAtUnixMs() != 1234 {
		t.Fatalf("expected failed_at_unix_ms=1234, got %d", e.GetFailedAtUnixMs())
	}
}
