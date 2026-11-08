package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type fakePrincipalNames map[string]string

func (f fakePrincipalNames) GetPrincipal(_ context.Context, id string) (domain.Principal, error) {
	name, ok := f[id]
	if !ok {
		return domain.Principal{}, domain.ErrPrincipalNotFound
	}
	return domain.Principal{ID: id, DisplayName: name}, nil
}

func runOneStepPlanWith(t *testing.T, s *Server, md map[string]string) map[string]string {
	t.Helper()
	auc := &capturingAuctioneer{}
	s.Dispatcher = auc
	_, _ = s.Execute(context.Background(), &pb.Handoff{
		Id:       "test-id",
		Payload:  &pb.Object{Data: []byte("summarise my unread email")},
		Metadata: md,
	})
	auc.mu.Lock()
	defer auc.mu.Unlock()
	if len(auc.received) == 0 {
		t.Fatal("no step was dispatched")
	}
	return auc.received[0].Context
}

// T-20261107-delegated-steps-name-the-conversation-owner-in: every dispatched step
// carries whom it acts for — the run principal, resolved by the SAME RunPrincipal
// function ExecuteTool uses for per_user credentials, with the step lease's
// binding shape (the run's session and run id) — plus the registry display name.
func TestExecute_StepCarriesTheRunPrincipalItActsFor(t *testing.T) {
	s := minimalServer(t)
	var seen domain.LeaseBinding
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		seen = b
		return "fenella"
	}
	s.PrincipalNames = fakePrincipalNames{"fenella": "Fenella Kirkwood"}

	got := runOneStepPlanWith(t, s, nil)
	if got[domain.HandoffKeyActingFor] != "fenella" || got[domain.HandoffKeyActingForName] != "Fenella Kirkwood" {
		t.Errorf("step context acting-for = %q / %q, want fenella / Fenella Kirkwood",
			got[domain.HandoffKeyActingFor], got[domain.HandoffKeyActingForName])
	}
	if seen.RunID == "" || seen.ConversationID != "" || seen.AgentID != "" {
		t.Errorf("RunPrincipal was not asked with a step-lease binding of this run: %+v", seen)
	}
}

// The registry not knowing the person withholds only the name, never the id.
func TestExecute_StepActingForWithoutARegistryName(t *testing.T) {
	s := minimalServer(t)
	s.RunPrincipal = func(context.Context, domain.LeaseBinding) string { return "fenella" }
	s.PrincipalNames = fakePrincipalNames{}

	got := runOneStepPlanWith(t, s, nil)
	if got[domain.HandoffKeyActingFor] != "fenella" {
		t.Errorf("acting-for id = %q, want fenella", got[domain.HandoffKeyActingFor])
	}
	if _, ok := got[domain.HandoffKeyActingForName]; ok {
		t.Errorf("unexpected display name %q", got[domain.HandoffKeyActingForName])
	}
}

// Kernel-authored: an Execute caller's metadata reaches the plan context, but it
// cannot assert whom a step acts for — no resolved principal removes the keys, and
// a resolved one overwrites them.
func TestExecute_CallerCannotAssertActingFor(t *testing.T) {
	spoof := map[string]string{domain.HandoffKeyActingFor: "mallory", domain.HandoffKeyActingForName: "Mallory"}

	s := minimalServer(t)
	got := runOneStepPlanWith(t, s, spoof)
	if v, ok := got[domain.HandoffKeyActingFor]; ok {
		t.Errorf("no run principal, yet the step acts for %q", v)
	}
	if v, ok := got[domain.HandoffKeyActingForName]; ok {
		t.Errorf("no run principal, yet the step carries the name %q", v)
	}

	s = minimalServer(t)
	s.RunPrincipal = func(context.Context, domain.LeaseBinding) string { return "fenella" }
	got = runOneStepPlanWith(t, s, spoof)
	if got[domain.HandoffKeyActingFor] != "fenella" {
		t.Errorf("acting-for = %q, want the kernel's fenella", got[domain.HandoffKeyActingFor])
	}
	if v, ok := got[domain.HandoffKeyActingForName]; ok {
		t.Errorf("the caller's name survived: %q", v)
	}
}
