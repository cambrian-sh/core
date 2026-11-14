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
	t.Parallel()
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	_, err := svc.ListWatchDeadLetters(context.Background(), &pb.ListWatchDeadLettersOpRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v", err)
	}
}

// With a reader wired, entries are mapped to the proto shape.
func TestDeadLetters_ReturnsMappedEntries(t *testing.T) {
	t.Parallel()
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

type fakeDeadLetterStore struct {
	fakeDeadLetterReader
	deleted []string
}

func (f *fakeDeadLetterStore) DeleteDeadLetter(id string) (bool, error) {
	for i, e := range f.entries {
		if e.ID == id {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			f.deleted = append(f.deleted, id)
			return true, nil
		}
	}
	return false, nil
}

// ENG-472: an operator deletes a dead letter they resolved — audited, with a
// reason, NotFound for an unknown id, Unimplemented when the reader cannot delete.
func TestDeleteWatchDeadLetter(t *testing.T) {
	ctx := operator.ContextWithPrincipal(context.Background(), "alice", operator.RoleOperator)
	svc, _, audit, _ := newCommandService()
	store := &fakeDeadLetterStore{fakeDeadLetterReader: fakeDeadLetterReader{entries: []domain.ReactiveDeadLetter{{ID: "dl-1"}, {ID: "dl-2"}}}}
	svc.SetDeadLetterReader(store)

	if _, err := svc.DeleteWatchDeadLetter(ctx, &pb.DeleteWatchDeadLetterOpRequest{CommandId: "c1", Reason: "handled by hand", DeadLetterId: "dl-1"}); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "dl-1" {
		t.Fatalf("deleted %v", store.deleted)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "c1"})
	if len(rows) != 1 || rows[0].ActionType != "delete_watch_dead_letter" {
		t.Fatalf("audit: %+v", rows)
	}
	if _, err := svc.DeleteWatchDeadLetter(ctx, &pb.DeleteWatchDeadLetterOpRequest{CommandId: "c2", Reason: "r", DeadLetterId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := svc.DeleteWatchDeadLetter(ctx, &pb.DeleteWatchDeadLetterOpRequest{CommandId: "c3", DeadLetterId: "dl-2"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a reason is required: %v", err)
	}
	readOnly, _, _, _ := newCommandService()
	readOnly.SetDeadLetterReader(&fakeDeadLetterReader{})
	if _, err := readOnly.DeleteWatchDeadLetter(ctx, &pb.DeleteWatchDeadLetterOpRequest{CommandId: "c4", Reason: "r", DeadLetterId: "x"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("a reader that cannot delete: %v", err)
	}
}
