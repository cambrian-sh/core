package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// Folding plan-state events yields the current in-flight set; the latest
// absolute state wins; re-applying is idempotent.
func TestProjection_FoldYieldsInFlightAndIsIdempotent(t *testing.T) {
	p := operator.NewProjection()

	p.Apply(domain.PlanStateChanged{PlanID: "p1", SessionID: "s1", Status: "running", ActiveStep: 0})
	p.Apply(domain.PlanStateChanged{PlanID: "p2", SessionID: "s2", Status: "running", ActiveStep: 0})
	// p1 advances (absolute-state: latest wins).
	p.Apply(domain.PlanStateChanged{PlanID: "p1", SessionID: "s1", Status: "running", ActiveStep: 3, CostSoFar: 0.42})
	// Re-apply the same event — must not duplicate or change the set.
	p.Apply(domain.PlanStateChanged{PlanID: "p1", SessionID: "s1", Status: "running", ActiveStep: 3, CostSoFar: 0.42})

	got := p.PlansInFlight()
	if len(got) != 2 {
		t.Fatalf("expected 2 plans in flight, got %d", len(got))
	}
	if got[0].PlanID != "p1" || got[0].ActiveStep != 3 || got[0].CostSoFar != 0.42 {
		t.Fatalf("expected p1 folded to step 3 / cost 0.42, got %+v", got[0])
	}
}

// A terminal event removes the plan from the in-flight set.
func TestProjection_TerminalRemovesPlan(t *testing.T) {
	p := operator.NewProjection()
	p.Apply(domain.PlanStateChanged{PlanID: "p1", Status: "running"})
	p.Apply(domain.PlanStateChanged{PlanID: "p1", Status: "completed", Terminal: true})

	if got := p.PlansInFlight(); len(got) != 0 {
		t.Fatalf("expected no plans in flight after terminal, got %+v", got)
	}
}

// fakeSessions is a SessionLister returning canned active/paused sessions.
type fakeSessions struct{ byStatus map[domain.SessionStatus][]domain.Session }

func (f fakeSessions) ListSessions(_ context.Context, s domain.SessionStatus) ([]domain.Session, error) {
	return f.byStatus[s], nil
}

// Snapshot stamps a lower-bound as_of_seq and fans in plans + sessions.
func TestService_SnapshotStampsSeqAndFansIn(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	proj := operator.NewProjection()
	svc := operator.NewService(feed)
	svc.SetSnapshotSources(proj, fakeSessions{byStatus: map[domain.SessionStatus][]domain.Session{
		domain.SessionActive: {{ID: "s1", Goal: "do x", Status: domain.SessionActive}},
	}})

	// Three events emitted ⇒ feed head 3; one plan in flight.
	feed.Emit(domain.AgentReadyEvent{})
	feed.Emit(domain.AgentReadyEvent{})
	feed.Emit(domain.AgentReadyEvent{})
	proj.Apply(domain.PlanStateChanged{PlanID: "p1", SessionID: "s1", Status: "running"})

	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if resp.GetAsOfSeq() != 3 {
		t.Fatalf("expected as_of_seq 3, got %d", resp.GetAsOfSeq())
	}
	if len(resp.GetPlans()) != 1 || resp.GetPlans()[0].GetPlanId() != "p1" {
		t.Fatalf("expected 1 plan p1, got %+v", resp.GetPlans())
	}
	if len(resp.GetSessions()) != 1 || resp.GetSessions()[0].GetId() != "s1" {
		t.Fatalf("expected 1 session s1, got %+v", resp.GetSessions())
	}
}

// Snapshot carries the capability + version handshake (ADR-0047 D14).
func TestService_SnapshotCarriesHandshake(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetHandshake("1.2.3", "0047", []string{"feed", "commands"})

	resp, err := svc.Snapshot(context.Background(), &pb.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if resp.GetKernelVersion() != "1.2.3" || resp.GetContractVersion() != "0047" {
		t.Fatalf("expected version handshake, got %q/%q", resp.GetKernelVersion(), resp.GetContractVersion())
	}
	if len(resp.GetCapabilities()) != 2 {
		t.Fatalf("expected 2 capabilities, got %+v", resp.GetCapabilities())
	}
}
