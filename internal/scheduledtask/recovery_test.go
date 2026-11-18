package scheduledtask_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261118-a-sync-approval-orphaned-by-a-kernel-restart: a schedule proposal
// approved after the proposing kernel run ended creates the task once, for its
// owner, within the caps in force now — or is refused with the reason.

type directory map[string]bool

func (d directory) KnownPrincipal(_ context.Context, id string) (bool, error) { return d[id], nil }

// proposeThenCrash raises the proposal on kernel run 1 and leaves the approval
// pending the way a killed process does.
func proposeThenCrash(t *testing.T, st *fakeStore, approvals domain.ApprovalStore) domain.ApprovalItem {
	t.Helper()
	h1 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: approvals, DefaultTimeout: time.Minute})
	s1 := svc(t, st, h1, domain.ScheduleLimits{MaxPerPrincipal: 1})
	ch, cancel := h1.Watch(domain.ApprovalWatch{For: domain.Approver{Principal: domain.PersonPrincipal("lena")}})
	defer cancel()
	ctx, die := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = s1.Propose(ctx, weekly("lena")); close(done) }()
	var it domain.ApprovalItem
	select {
	case ev := <-ch:
		it = ev.Item
	case <-time.After(3 * time.Second):
		t.Fatal("no approval raised")
	}
	die()
	<-done
	if it.Intent == nil || it.Intent.Executor != domain.ApprovalIntentSchedule || it.Intent.OnBehalfOf != "lena" {
		t.Fatalf("the schedule approval stored no intent: %+v", it.Intent)
	}
	it.State = domain.ApprovalPending
	if err := approvals.PutApproval(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	return it
}

func restartedHub(t *testing.T, st *fakeStore, approvals domain.ApprovalStore, actions domain.ActionRecordStore, dir domain.PrincipalDirectory) *domain.AddressedHub {
	t.Helper()
	h2 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: approvals, ActionRecords: actions})
	s2 := svc(t, st, h2, domain.ScheduleLimits{MaxPerPrincipal: 1})
	h2.RecoverWith(domain.ApprovalIntentSchedule, s2.ApprovalRecoverer(func() domain.PrincipalDirectory { return dir }))
	return h2
}

var lena = domain.Approver{Principal: domain.PersonPrincipal("lena")}

func TestRecovery_ApprovedAfterRestart_CreatesOnce(t *testing.T) {
	st, approvals, actions := newFakeStore(), domain.NewMemoryApprovalStore(), domain.NewMemActionRecordStore()
	it := proposeThenCrash(t, st, approvals)
	if tasks, _ := st.List(context.Background(), "lena"); len(tasks) != 0 {
		t.Fatal("created before the approve")
	}
	h2 := restartedHub(t, st, approvals, actions, directory{"lena": true})
	res, err := h2.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: lena})
	if err != nil || !strings.Contains(res.Detail, "created scheduled task") {
		t.Fatalf("approve after restart: %+v %v", res, err)
	}
	if _, err := h2.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: lena}); !errors.Is(err, domain.ErrApprovalResolved) {
		t.Fatalf("second approve: %v", err)
	}
	tasks, _ := st.List(context.Background(), "lena")
	if len(tasks) != 1 || tasks[0].OwnerID != "lena" || tasks[0].ApprovedBy != "lena" || tasks[0].Goal != "Summarise my inbox notes" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestRecovery_OwnerGoneOrCapReached_Refused(t *testing.T) {
	st, approvals, actions := newFakeStore(), domain.NewMemoryApprovalStore(), domain.NewMemActionRecordStore()
	it := proposeThenCrash(t, st, approvals)
	h2 := restartedHub(t, st, approvals, actions, directory{})
	res, err := h2.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: lena})
	if !errors.Is(err, domain.ErrApprovalNotCarriedOut) || res.State != domain.ApprovalWithdrawn ||
		!strings.Contains(err.Error(), "no longer a known, active person") {
		t.Fatalf("approve for an owner who left: %+v %v", res, err)
	}

	st2, approvals2 := newFakeStore(), domain.NewMemoryApprovalStore()
	it2 := proposeThenCrash(t, st2, approvals2)
	// While the kernel was down the owner reached the cap.
	if _, err := st2.Create(context.Background(), domain.ScheduledTaskSpec{OwnerID: "lena", Title: "other", Goal: "x", Cron: "0 9 * * *"},
		domain.ScheduleLimits{}); err != nil {
		t.Fatal(err)
	}
	h3 := restartedHub(t, st2, approvals2, domain.NewMemActionRecordStore(), directory{"lena": true})
	res, err = h3.Answer(context.Background(), it2.ID, domain.ApprovalAnswer{Approved: true, By: lena})
	if !errors.Is(err, domain.ErrApprovalNotCarriedOut) || res.State != domain.ApprovalWithdrawn {
		t.Fatalf("approve over the cap: %+v %v", res, err)
	}
	if tasks, _ := st2.List(context.Background(), "lena"); len(tasks) != 1 {
		t.Fatalf("a task over the cap was created: %+v", tasks)
	}
}

func TestProposalIntent_RoundTrips(t *testing.T) {
	ap := &fakeApprovals{dec: approve}
	if _, err := svc(t, newFakeStore(), ap, domain.ScheduleLimits{}).Propose(context.Background(), weekly("lena")); err != nil {
		t.Fatal(err)
	}
	in := ap.reqs[0].Intent
	if in == nil || in.Tool != "schedule_task" || !strings.Contains(string(in.Args), `"owner_id":"lena"`) {
		t.Fatalf("intent = %+v", in)
	}
}
