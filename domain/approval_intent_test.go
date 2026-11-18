package domain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// T-20261118-a-sync-approval-orphaned-by-a-kernel-restart: an approval approved
// after the kernel run that raised it ended carries out its action once, or is
// refused with the reason — never "approved" with nothing run.

type fakeRecoverer struct {
	mu       sync.Mutex
	checkErr error
	runErr   error
	runs     int
	outcome  string
}

func (f *fakeRecoverer) CheckRecovery(context.Context, ApprovalItem) error { return f.checkErr }
func (f *fakeRecoverer) Recover(context.Context, ApprovalItem) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.runErr != nil {
		return "", f.runErr
	}
	return f.outcome, nil
}
func (f *fakeRecoverer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.runs }

type fakeDirectory map[string]bool

func (d fakeDirectory) KnownPrincipal(_ context.Context, id string) (bool, error) { return d[id], nil }

var orphanAlice = PersonPrincipal("alice")

func aliceApprover() Approver { return Approver{Principal: orphanAlice} }

// putOrphan stores a pending sync item as an earlier kernel run left it.
func putOrphan(t *testing.T, store ApprovalStore, id string, intent *ApprovalIntent) ApprovalItem {
	t.Helper()
	now := time.Now()
	it := ApprovalItem{ID: id, Kind: ApprovalKindSchedule, ToolName: "schedule_task", State: ApprovalPending,
		NaturalKey: "k-" + id, RaisedAt: now, ExpiresAt: now.Add(time.Hour), Beneficiary: orphanAlice,
		Rules: []ApprovalRule{{Kind: ApprovalRuleSelf}}, Intent: intent}
	if err := store.PutApproval(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	return it
}

func scheduleIntent() *ApprovalIntent {
	return &ApprovalIntent{Executor: ApprovalIntentSchedule, Tool: "schedule_task", Args: []byte(`{"goal":"g"}`), OnBehalfOf: "alice"}
}

func TestOrphanApprove_CarriedOutOnce(t *testing.T) {
	store, actions := NewMemoryApprovalStore(), NewMemActionRecordStore()
	log, bus := newBusLog()
	h := NewAddressedHub(AddressedHubConfig{Store: store, ActionRecords: actions, Bus: bus})
	rec := &fakeRecoverer{outcome: "created scheduled task sched-1"}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	it := putOrphan(t, store, "apr-o1", scheduleIntent())

	res, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if err != nil || res.State != ApprovalApproved || !strings.Contains(res.Detail, "approved and carried out: created scheduled task sched-1") {
		t.Fatalf("approve after restart: %+v %v", res, err)
	}
	// Approving again changes nothing and runs nothing.
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("second approve: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("recoverer ran %d times, want 1", rec.count())
	}
	stored, _ := store.GetApproval(context.Background(), it.ID)
	if stored.Intent != nil || !stored.Consumed || stored.State != ApprovalApproved || !strings.Contains(stored.Detail, "carried out") {
		t.Fatalf("stored after recovery: %+v", stored)
	}
	ar, err := actions.Get(context.Background(), RecoveryActionKey(it.ID))
	if err != nil || ar.State != ActionStateAccepted {
		t.Fatalf("action record: %+v %v", ar, err)
	}
	// The person's feed hears the approval and then its outcome.
	if outs := log.outcomes(); len(outs) != 2 || outs[0] != "approved" || outs[1] != "approved" {
		t.Fatalf("published outcomes: %v", outs)
	}
	// A later identical request is not handed the recovered decision.
	h2 := NewAddressedHub(AddressedHubConfig{Store: store, ActionRecords: actions})
	if _, err := h2.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("approve on another kernel: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("recoverer ran again: %d", rec.count())
	}
}

func TestOrphanApprove_CannotBeDoneSafely_RefusedAndWithdrawn(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store})
	rec := &fakeRecoverer{checkErr: errors.New("alice is no longer a known, active person")}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	it := putOrphan(t, store, "apr-o2", scheduleIntent())

	res, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || res.State != ApprovalWithdrawn ||
		!strings.Contains(err.Error(), "no longer a known, active person") || !strings.Contains(res.Detail, "nothing was done") {
		t.Fatalf("unsafe approve must be refused: %+v %v", res, err)
	}
	if rec.count() != 0 {
		t.Fatal("a refused intent ran")
	}
	if stored, _ := store.GetApproval(context.Background(), it.ID); stored.Intent != nil {
		t.Fatalf("a withdrawn item keeps its intent: %+v", stored)
	}
}

func TestOrphanApprove_NoRecoverer_Refused(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store})
	it := putOrphan(t, store, "apr-o3", scheduleIntent()) // nothing registered for "schedule"
	res, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || res.State != ApprovalWithdrawn {
		t.Fatalf("approve with no recoverer: %+v %v", res, err)
	}
}

func TestOrphanApprove_RecoveryFails_ApproverIsTold(t *testing.T) {
	store, actions := NewMemoryApprovalStore(), NewMemActionRecordStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store, ActionRecords: actions})
	h.RecoverWith(ApprovalIntentSchedule, &fakeRecoverer{runErr: errors.New("the schedule store is unavailable")})
	it := putOrphan(t, store, "apr-o4", scheduleIntent())
	_, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || !strings.Contains(err.Error(), "carrying it out failed") {
		t.Fatalf("a failed recovery must not answer ok: %v", err)
	}
	ar, _ := actions.Get(context.Background(), RecoveryActionKey(it.ID))
	if ar.State != ActionStateDispatched || ar.Error == "" {
		t.Fatalf("an unknown outcome stays dispatched with its error: %+v", ar)
	}

	h.RecoverWith(ApprovalIntentSchedule, &fakeRecoverer{runErr: errors.Join(ErrRecoveryRefused, errors.New("limit"))})
	it5 := putOrphan(t, store, "apr-o5", scheduleIntent())
	_, err = h.Answer(context.Background(), it5.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || !strings.Contains(err.Error(), "was not carried out") {
		t.Fatalf("a refused recovery: %v", err)
	}
	if ar, _ := actions.Get(context.Background(), RecoveryActionKey(it5.ID)); ar.State != ActionStateNotConfirmed {
		t.Fatalf("a refusal settles not_confirmed: %+v", ar)
	}
}

func TestOrphanDeny_RecordedWithoutRecovery(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store})
	rec := &fakeRecoverer{}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	it := putOrphan(t, store, "apr-o6", scheduleIntent())
	res, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: false, By: aliceApprover()})
	if err != nil || res.State != ApprovalDenied || rec.count() != 0 {
		t.Fatalf("deny of an orphan: %+v %v runs=%d", res, err, rec.count())
	}
}

// A live requester in this process gets the answer itself; the recoverer is
// never involved.
func TestLiveRequester_NotRecovered(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store, DefaultTimeout: time.Minute})
	rec := &fakeRecoverer{}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	ch, cancel := h.Watch(ApprovalWatch{For: aliceApprover()})
	defer cancel()
	done := raiseAsync(context.Background(), h, ApprovalRequest{Kind: ApprovalKindSchedule, ToolName: "schedule_task",
		Beneficiary: orphanAlice, Intent: scheduleIntent()})
	it := nextRaised(t, ch)
	if it.Intent == nil {
		t.Fatal("a sync request's intent must be stored with its item")
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()}); err != nil {
		t.Fatal(err)
	}
	if d := <-done; !d.Approved {
		t.Fatalf("requester: %+v", d)
	}
	if rec.count() != 0 {
		t.Fatal("a live requester's approval was recovered too")
	}
	if stored, _ := store.GetApproval(context.Background(), it.ID); stored.Intent != nil {
		t.Fatalf("a resolved item keeps its intent: %+v", stored)
	}
}

// ENG-2119 compatibility: an approval withdrawn because its requester stopped
// waiting (its chat turn ended) is final — an approve is refused and nothing runs.
func TestWithdrawnApproval_NeverRecovered(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store, DefaultTimeout: time.Minute})
	rec := &fakeRecoverer{}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	ch, cancel := h.Watch(ApprovalWatch{For: aliceApprover()})
	defer cancel()
	ctx, end := context.WithCancel(context.Background())
	done := raiseAsync(ctx, h, ApprovalRequest{Kind: ApprovalKindSchedule, ToolName: "schedule_task",
		Beneficiary: orphanAlice, Intent: scheduleIntent()})
	it := nextRaised(t, ch)
	end()
	<-done
	// A restarted kernel sees the same withdrawn row.
	h2 := NewAddressedHub(AddressedHubConfig{Store: store})
	h2.RecoverWith(ApprovalIntentSchedule, rec)
	res, err := h2.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: aliceApprover()})
	if !errors.Is(err, ErrApprovalResolved) || res.State != ApprovalWithdrawn || rec.count() != 0 {
		t.Fatalf("withdrawn approval approved after restart: %+v %v runs=%d", res, err, rec.count())
	}
}

// A crash between recording the approve and finishing the action: the next
// boot finishes it when the action record says it never ran, and reports the
// outcome as unknown when it was already dispatched.
func TestStart_ResumesInterruptedRecovery(t *testing.T) {
	store, actions := NewMemoryApprovalStore(), NewMemActionRecordStore()
	ctx := context.Background()
	approved := func(id string) ApprovalItem {
		it := putOrphan(t, store, id, scheduleIntent())
		it.State, it.ResolvedBy, it.ResolvedAt, it.Consumed = ApprovalApproved, "alice", time.Now(), true
		_ = store.PutApproval(ctx, it)
		return it
	}
	never := approved("apr-r1")
	sent := approved("apr-r2")
	if _, _, err := actions.Record(ctx, DurableAction{Key: RecoveryActionKey(sent.ID), State: ActionStateDispatched}); err != nil {
		t.Fatal(err)
	}
	h := NewAddressedHub(AddressedHubConfig{Store: store, ActionRecords: actions, SweepInterval: time.Hour})
	rec := &fakeRecoverer{outcome: "done"}
	h.RecoverWith(ApprovalIntentSchedule, rec)
	h.Start(ctx)
	h.Stop()
	if rec.count() != 1 {
		t.Fatalf("recoverer ran %d times, want 1 (only the never-dispatched one)", rec.count())
	}
	a, _ := store.GetApproval(ctx, never.ID)
	b, _ := store.GetApproval(ctx, sent.ID)
	if a.Intent != nil || !strings.Contains(a.Detail, "carried out: done") {
		t.Fatalf("never-dispatched: %+v", a)
	}
	if b.Intent != nil || !strings.Contains(b.Detail, "outcome is unknown") {
		t.Fatalf("dispatched before the crash: %+v", b)
	}
}

// The intent rides only on an item persisted pending for a blocking requester.
func TestIntent_NotStoredOnAsyncOrStanding(t *testing.T) {
	store := NewMemoryApprovalStore()
	h := NewAddressedHub(AddressedHubConfig{Store: store})
	it, err := h.Raise(context.Background(), ApprovalRequest{Kind: ApprovalKindPublish, ToolName: "publish",
		Beneficiary: orphanAlice, Intent: scheduleIntent()})
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := store.GetApproval(context.Background(), it.ID); stored.Intent != nil {
		t.Fatalf("an async item stored an intent: %+v", stored)
	}
}
