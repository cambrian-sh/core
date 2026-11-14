package scheduledtask_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scheduledtask"
)

// fakeStore is an in-memory domain.PrincipalSchedules honouring the caps the way
// the premium store does (count per owner; the interval check is the premium
// store's cron parser, faked here by a sentinel cron).
type fakeStore struct {
	mu    sync.Mutex
	tasks map[string]domain.ScheduledTask
}

func newFakeStore() *fakeStore { return &fakeStore{tasks: map[string]domain.ScheduledTask{}} }

func (f *fakeStore) Check(_ context.Context, sp domain.ScheduledTaskSpec, l domain.ScheduleLimits) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sp.Cron == "* * * * *" && l.MinInterval > time.Minute {
		return domain.ErrScheduleTooFrequent
	}
	if _, exists := f.tasks[domain.ScheduledTaskID(sp)]; exists {
		return nil
	}
	n := 0
	for _, t := range f.tasks {
		if t.OwnerID == sp.OwnerID {
			n++
		}
	}
	if l.MaxPerPrincipal > 0 && n >= l.MaxPerPrincipal {
		return domain.ErrScheduleLimit
	}
	return nil
}

func (f *fakeStore) Create(ctx context.Context, sp domain.ScheduledTaskSpec, l domain.ScheduleLimits) (domain.ScheduledTask, error) {
	if err := f.Check(ctx, sp, l); err != nil {
		return domain.ScheduledTask{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := domain.ScheduledTaskID(sp)
	t := domain.ScheduledTask{ID: id, OwnerID: sp.OwnerID, Title: sp.Title, Goal: sp.Goal, Cron: sp.Cron,
		Delivery: sp.Delivery, Active: true, ApprovedBy: sp.ApprovedBy, ConversationID: domain.ScheduledTaskConversationID(id)}
	f.tasks[id] = t
	return t, nil
}

func (f *fakeStore) List(_ context.Context, owner string) ([]domain.ScheduledTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.ScheduledTask
	for _, t := range f.tasks {
		if owner == "" || t.OwnerID == owner {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) SetActive(_ context.Context, owner, id string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok || (owner != "" && t.OwnerID != owner) {
		return domain.ErrScheduleNotFound
	}
	t.Active = active
	f.tasks[id] = t
	return nil
}

func (f *fakeStore) Delete(_ context.Context, owner, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok || (owner != "" && t.OwnerID != owner) {
		return domain.ErrScheduleNotFound
	}
	delete(f.tasks, id)
	return nil
}

// fakeApprovals answers every request with a fixed decision and records it.
type fakeApprovals struct {
	dec  domain.ApprovalDecision
	reqs []domain.ApprovalRequest
}

func (a *fakeApprovals) Request(_ context.Context, r domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	a.reqs = append(a.reqs, r)
	return a.dec, nil
}

var approve = domain.ApprovalDecision{Approved: true, Decided: true, ApproverID: "lena"}

func svc(t *testing.T, st domain.PrincipalSchedules, ap domain.ApprovalController, l domain.ScheduleLimits) *scheduledtask.Service {
	t.Helper()
	s, err := scheduledtask.New(st, ap, l, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func weekly(owner string) scheduledtask.Proposal {
	return scheduledtask.Proposal{OwnerID: owner, AgentID: "assistant", Title: "weekly summary",
		Goal: "Summarise my inbox notes", RRule: "FREQ=WEEKLY;BYDAY=MO;BYHOUR=8;BYMINUTE=0"}
}

func TestPropose_ApprovedCreatesOwnedByThePrincipal(t *testing.T) {
	st, ap := newFakeStore(), &fakeApprovals{dec: approve}
	s := svc(t, st, ap, domain.ScheduleLimits{MaxPerPrincipal: 5, MinInterval: time.Hour})
	out, err := s.Propose(context.Background(), weekly("lena"))
	if err != nil || !out.Created {
		t.Fatalf("approved proposal: %+v err=%v", out, err)
	}
	if out.Task.OwnerID != "lena" || out.Task.Cron != "0 8 * * 1" || out.Task.ApprovedBy != "lena" {
		t.Fatalf("task = %+v", out.Task)
	}
	if out.Task.ConversationID != domain.ScheduledTaskConversationID(out.Task.ID) {
		t.Fatalf("conversation id %q", out.Task.ConversationID)
	}
	if len(ap.reqs) != 1 || ap.reqs[0].Kind != domain.ApprovalKindSchedule || ap.reqs[0].ToolName != "schedule_task" ||
		!strings.HasPrefix(ap.reqs[0].ArgsPreview, "for lena:") {
		t.Fatalf("approval request = %+v", ap.reqs)
	}
	// Deterministic: the same proposal again is the same task, not a duplicate.
	again, _ := s.Propose(context.Background(), weekly("lena"))
	if again.Task.ID != out.Task.ID {
		t.Fatalf("re-proposal minted a new id: %q vs %q", again.Task.ID, out.Task.ID)
	}
	if all, _ := st.List(context.Background(), ""); len(all) != 1 {
		t.Fatalf("want 1 task, got %d", len(all))
	}
}

func TestPropose_DeniedOrUnansweredCreatesNothing(t *testing.T) {
	for name, dec := range map[string]domain.ApprovalDecision{
		"denied":      {Approved: false, Decided: true, ApproverID: "lena"},
		"no-decision": {Approved: false, Decided: false},
	} {
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			s := svc(t, st, &fakeApprovals{dec: dec}, domain.ScheduleLimits{})
			out, err := s.Propose(context.Background(), weekly("lena"))
			if err != nil || out.Created {
				t.Fatalf("%s: %+v err=%v", name, out, err)
			}
			want := scheduledtask.RejectDenied
			if name == "no-decision" {
				want = scheduledtask.RejectNoDecision
			}
			if out.Rejection != want {
				t.Fatalf("rejection %v, want %v", out.Rejection, want)
			}
			if all, _ := st.List(context.Background(), ""); len(all) != 0 {
				t.Fatalf("nothing may be created: %+v", all)
			}
		})
	}
}

func TestPropose_CapsRefusedBeforeAsking(t *testing.T) {
	st, ap := newFakeStore(), &fakeApprovals{dec: approve}
	s := svc(t, st, ap, domain.ScheduleLimits{MaxPerPrincipal: 1, MinInterval: time.Hour})
	if out, _ := s.Propose(context.Background(), weekly("lena")); !out.Created {
		t.Fatalf("first: %+v", out)
	}
	p := weekly("lena")
	p.Goal = "another"
	out, _ := s.Propose(context.Background(), p)
	if out.Created || out.Rejection != scheduledtask.RejectLimit {
		t.Fatalf("over the per-principal cap: %+v", out)
	}
	// Another principal has their own cap.
	if out, _ := s.Propose(context.Background(), weekly("dana")); !out.Created {
		t.Fatalf("dana's first: %+v", out)
	}
	every := scheduledtask.Proposal{OwnerID: "dana", Goal: "x", Cron: "* * * * *"}
	out, _ = s.Propose(context.Background(), every)
	if out.Created || out.Rejection != scheduledtask.RejectTooFrequent {
		t.Fatalf("below the minimum interval: %+v", out)
	}
	if len(ap.reqs) != 2 {
		t.Fatalf("refused proposals must not reach the approver: %d requests", len(ap.reqs))
	}
}

func TestPropose_InvalidAndNoPrincipal(t *testing.T) {
	s := svc(t, newFakeStore(), &fakeApprovals{dec: approve}, domain.ScheduleLimits{})
	for name, p := range map[string]scheduledtask.Proposal{
		"no goal":      {OwnerID: "o", Cron: "@daily"},
		"both":         {OwnerID: "o", Goal: "g", Cron: "@daily", RRule: "FREQ=DAILY"},
		"neither":      {OwnerID: "o", Goal: "g"},
		"bad rrule":    {OwnerID: "o", Goal: "g", RRule: "FREQ=DAILY;COUNT=3"},
		"bad delivery": {OwnerID: "o", Goal: "g", Cron: "@daily", Delivery: "fax"},
		// ENG-753: the deprecated agent_id was recorded and never routed; a
		// caller is no longer told it was accepted.
		"agent_id": {OwnerID: "o", Goal: "g", Cron: "@daily", RunAgent: "research_agent"},
	} {
		if out, _ := s.Propose(context.Background(), p); out.Created || out.Rejection != scheduledtask.RejectInvalid {
			t.Errorf("%s: %+v", name, out)
		}
	}
	if out, _ := s.Propose(context.Background(), scheduledtask.Proposal{Goal: "g", Cron: "@daily"}); out.Rejection != scheduledtask.RejectNoPrincipal {
		t.Fatalf("no owner: %+v", out)
	}
}

func TestOwnerScopedListPauseDelete(t *testing.T) {
	st := newFakeStore()
	s := svc(t, st, &fakeApprovals{dec: approve}, domain.ScheduleLimits{})
	out, _ := s.Propose(context.Background(), weekly("lena"))
	id := out.Task.ID
	if err := s.SetActive(context.Background(), "dana", id, false); err != domain.ErrScheduleNotFound {
		t.Fatalf("another principal pausing lena's task: %v", err)
	}
	if err := s.SetActive(context.Background(), "lena", id, false); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(context.Background(), "lena"); len(l) != 1 || l[0].Active {
		t.Fatalf("paused list: %+v", l)
	}
	if l, _ := s.List(context.Background(), "dana"); len(l) != 0 {
		t.Fatalf("dana sees lena's: %+v", l)
	}
	if _, err := s.List(context.Background(), ""); err == nil {
		t.Fatal("the agent-plane list must never list everyone's")
	}
	if err := s.Delete(context.Background(), "dana", id); err != domain.ErrScheduleNotFound {
		t.Fatalf("another principal deleting: %v", err)
	}
	if err := s.Delete(context.Background(), "lena", id); err != nil {
		t.Fatal(err)
	}
}

func TestNew_RefusesWithoutApprovalChannel(t *testing.T) {
	if _, err := scheduledtask.New(newFakeStore(), nil, domain.ScheduleLimits{}, 0); err == nil {
		t.Fatal("a service without an approval channel would create silently")
	}
}
