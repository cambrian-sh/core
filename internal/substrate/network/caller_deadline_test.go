package network

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// A chat turn delegates with a gRPC deadline (its time left minus a reply reserve,
// agents/chat_agent.py). A plan that ran INTO that deadline died with its RPC: the
// caller got DEADLINE_EXCEEDED and no payload, although step 0 had finished and
// written (run 20261107T120552Z_usecases_full: a cases.decide call never reached the
// reply). The plan is now stopped execution.plan.caller_deadline_reserve_ms before the
// caller's deadline and Execute answers — before the deadline — with the completed
// step's result and the ledger of its writes, marked as a timed-out partial plan
// (T-20261107-a-delegated-plan-stopped-by-its-caller-s).
func TestExecute_PlanStopsBeforeTheCallersDeadlineAndReportsWhatItDid(t *testing.T) {
	t.Parallel()
	var s *Server
	caller := callerFunc(func(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
		b, _ := s.leaseResolver().ResolveLease(stepLease(h))
		if b.StepIndex == 0 {
			// The write the reply must not lose, recorded as ExecuteTool records it.
			s.planActions.record(b.RunID, domain.PlanAction{Step: 0, Agent: agentID, Tool: "cases_decide",
				Status: domain.PlanActionDone, IDs: []string{"case=42"}})
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("case 42 approved")}, FromAgent: agentID}, nil
		}
		<-ctx.Done() // step 1 is still working when time runs out
		return nil, ctx.Err()
	})
	s = watchdogServer(t, 2, config.PlanConfig{PlanMaxDurationMs: 60000, CallerDeadlineReserveMs: 300}, caller, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	resp, err := s.Execute(ctx, &pb.Handoff{Payload: &pb.Object{Data: []byte("decide case 42 and notify")}})
	if ctx.Err() != nil {
		t.Fatalf("Execute returned after its caller's deadline (%v); the answer would be lost on the wire", ctx.Err())
	}
	if err != nil {
		t.Fatalf("Execute: %v — want the partial report, not an error", err)
	}
	if resp.Metadata[PlanTimedOutKey] != "true" || resp.Metadata["_partial_plan"] != "true" {
		t.Fatalf("want a timed-out partial plan, got metadata %v", resp.Metadata)
	}
	if c := resp.Metadata[PlanTimeoutCauseKey]; !strings.Contains(c, "execution.plan.caller_deadline_reserve_ms") {
		t.Errorf("cause %q does not name the reserve key", c)
	}
	payload := string(resp.Payload.Data)
	for _, want := range []string{"The plan did not finish: it ran out of time at step 1", "case 42 approved",
		domain.PlanActionsHeader, "cases_decide · done · ids: case=42"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %q:\n%s", want, payload)
		}
	}
	if !strings.Contains(resp.Metadata[PlanActionsKey], "case=42") {
		t.Errorf("ledger metadata = %q", resp.Metadata[PlanActionsKey])
	}
}

// The reserve never stops a plan before it starts: a caller granting less than twice
// the reserve gets half its time, one with no deadline is untouched, and 0 disables it.
func TestWithCallerDeadlineReserve(t *testing.T) {
	t.Parallel()
	if c, stop := withCallerDeadlineReserve(context.Background(), time.Second); c != context.Background() {
		stop()
		t.Fatal("a caller with no deadline must be untouched")
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if c, _ := withCallerDeadlineReserve(parent, 0); c != parent {
		t.Fatal("reserve 0 must disable it")
	}
	pd, _ := parent.Deadline()
	c, stop := withCallerDeadlineReserve(parent, 5*time.Second)
	defer stop()
	if d, _ := c.Deadline(); pd.Sub(d) != 5*time.Second {
		t.Fatalf("deadline moved by %s, want 5s", pd.Sub(d))
	}
	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()
	sd, _ := short.Deadline()
	c2, stop2 := withCallerDeadlineReserve(short, 5*time.Second)
	defer stop2()
	if d, _ := c2.Deadline(); sd.Sub(d) > 1100*time.Millisecond || c2.Err() != nil {
		t.Fatalf("short caller: deadline moved by %s (err %v), want about half its time", sd.Sub(d), c2.Err())
	}
}

// withCallerDeadlineReserve bounds ctx by a reserve computed from ctx itself — the
// shape Execute used before the reserve was fixed once per Execute (callerReserve).
func withCallerDeadlineReserve(ctx context.Context, reserve time.Duration) (context.Context, context.CancelFunc) {
	r := newCallerReserve(ctx, reserve)
	return r.bound(ctx, errPlanCallerDeadline(r.reserve))
}
