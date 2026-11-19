package network

import (
	"context"
	"errors"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ENG-1408: Server.Execute is a shared prologue plus a lane switch. These pin the
// two seams the split introduced; the lanes themselves stay covered end to end by
// the single-agent, plan, router, watchdog and delegation suites in this package.

// The prologue's cleanups used to be defers in Execute: they still run last
// registered first, and the result cleanup sees Execute's final handoff.
func TestExecuteFinish_RunsLastRegisteredFirstWithTheResult(t *testing.T) {
	var order []string
	var seen *pb.Handoff
	f := &executeFinish{}
	f.push(func() { order = append(order, "worker-delegation") })
	f.pushResult(func(out *pb.Handoff) { seen = out; order = append(order, "turn-delegation") })
	f.push(func() { order = append(order, "cancel-tracking") })

	out := &pb.Handoff{Id: "final"}
	f.run(out)
	want := []string{"cancel-tracking", "turn-delegation", "worker-delegation"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if seen != out {
		t.Fatal("the result cleanup did not see Execute's final handoff")
	}
	(&executeFinish{}).run(nil) // nothing registered: a no-op
}

// The planner lane's routing step answers CHAT, WATCH,
// clarifications and router errors itself (done), and lets PLAN continue into
// the planner lane (not done).
func TestRoutePlannerRequest_AnswersOrContinuesByDecision(t *testing.T) {
	in := &pb.Handoff{Id: "h1", Payload: &pb.Object{Data: []byte("do the thing")}}
	for _, tc := range []struct {
		name     string
		router   *stubRouter
		done     bool
		wantType string
		wantErr  bool
	}{
		{"plan continues", routerReturning(domain.DecisionPlan), false, "", false},
		{"chat", routerReturning(domain.DecisionChat), true, "not_implemented", false},
		{"watch", routerReturning(domain.DecisionWatch), true, "not_implemented", false},
		{"clarification", routerReturningClarification(), true, "", false},
		{"router error", &stubRouter{err: errors.New("router down")}, true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Router: tc.router}
			ctx := context.Background()
			out, done, err := s.routePlannerRequest(ctx, ctx, in, "do the thing", callerReserve{}, "", llm.SessionSpend{})
			if done != tc.done {
				t.Fatalf("done = %v, want %v (out=%v err=%v)", done, tc.done, out, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.done && (out != nil || err != nil) {
				t.Fatalf("a continuing request must carry no result: out=%v err=%v", out, err)
			}
			if tc.wantType != "" && out.GetPayload().GetType() != tc.wantType {
				t.Fatalf("payload type = %q, want %q", out.GetPayload().GetType(), tc.wantType)
			}
			if tc.name == "clarification" && out == nil {
				t.Fatal("a clarification must be answered")
			}
		})
	}
}
