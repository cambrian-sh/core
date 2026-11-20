package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// fakeRetrier replays a failed-delivery entry once and removes it, as the premium
// engine does: a second retry finds nothing.
type fakeRetrier struct {
	entries  map[string]domain.ReactiveDeadLetter
	replayed []string
	avail    bool
}

func (f *fakeRetrier) RetryAvailable() bool { return f.avail }

func (f *fakeRetrier) RetryDeadLetter(_ context.Context, id string) error {
	if !f.avail {
		return domain.ErrDeadLetterRetryUnavailable
	}
	dl, ok := f.entries[id]
	if !ok {
		return domain.ErrDeadLetterNotFound
	}
	if !dl.Retryable() {
		return domain.ErrDeadLetterNotRetryable
	}
	f.replayed = append(f.replayed, id)
	delete(f.entries, id)
	return nil
}

// ENG-1654 (owner decision 2026-11-20, option C): a failed-delivery dead letter is
// replayed once through the operator RPC (audited), a second retry is NotFound, an
// expired signal is FailedPrecondition (delete-only), and a kernel with no retrier
// answers Unimplemented. The list marks exactly the entries a retry can replay.
func TestRetryWatchDeadLetter_ReplaysFailedDeliveriesOnly(t *testing.T) {
	ctx := operator.ContextWithPrincipal(context.Background(), "alice", operator.RoleOperator)
	svc, _, audit, _ := newCommandService()
	queued := domain.ReactiveDeadLetter{ID: "dl-queue", WatchID: "w1", Reason: "queue_full"}
	expired := domain.ReactiveDeadLetter{ID: "dl-old", WatchID: "w1", Reason: domain.DeadLetterReasonExpired}
	r := &fakeRetrier{avail: true, entries: map[string]domain.ReactiveDeadLetter{queued.ID: queued, expired.ID: expired}}
	svc.SetDeadLetterRetrier(r)
	svc.SetDeadLetterReader(&fakeDeadLetterReader{entries: []domain.ReactiveDeadLetter{queued, expired}})

	list, err := svc.ListWatchDeadLetters(ctx, &pb.ListWatchDeadLettersOpRequest{})
	if err != nil || len(list.GetEntries()) != 2 || !list.GetEntries()[0].GetRetryable() || list.GetEntries()[1].GetRetryable() {
		t.Fatalf("list retryable flags: %+v %v", list.GetEntries(), err)
	}

	if _, err := svc.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{CommandId: "c1", Reason: "queue drained", DeadLetterId: "dl-queue"}); err != nil {
		t.Fatal(err)
	}
	if len(r.replayed) != 1 {
		t.Fatalf("replayed %v, want once", r.replayed)
	}
	if rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "c1"}); len(rows) != 1 || rows[0].ActionType != "retry_watch_dead_letter" {
		t.Fatalf("audit: %+v", rows)
	}
	// The same command resent is deduped; a new retry finds nothing to replay.
	if _, err := svc.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{CommandId: "c1", Reason: "queue drained", DeadLetterId: "dl-queue"}); err != nil || len(r.replayed) != 1 {
		t.Fatalf("resent command: err=%v replayed=%v", err, r.replayed)
	}
	if _, err := svc.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{CommandId: "c2", Reason: "again", DeadLetterId: "dl-queue"}); status.Code(err) != codes.NotFound || len(r.replayed) != 1 {
		t.Fatalf("second retry: %v replayed=%v, want NotFound and no second replay", err, r.replayed)
	}
	if _, err := svc.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{CommandId: "c3", Reason: "r", DeadLetterId: "dl-old"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expired signal: %v, want FailedPrecondition", err)
	}

	// No retrier registered (the late-bound holder empty): Unimplemented, nothing retryable.
	r.avail = false
	if _, err := svc.RetryWatchDeadLetter(ctx, &pb.RetryWatchDeadLetterOpRequest{CommandId: "c4", Reason: "r", DeadLetterId: "dl-old"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no retrier: %v, want Unimplemented", err)
	}
	list, _ = svc.ListWatchDeadLetters(ctx, &pb.ListWatchDeadLettersOpRequest{})
	for _, e := range list.GetEntries() {
		if e.GetRetryable() {
			t.Fatalf("entry %s offered as retryable with no retrier", e.GetId())
		}
	}
}
