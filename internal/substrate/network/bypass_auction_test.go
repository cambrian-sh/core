package network

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ADR-0050 D1: bypass_auction dispatches the input verbatim to the configured
// single agent, strips the benchmark's "/plan " routing prefix, and maps the
// agent's handoff straight back to the caller.
func TestExecute_BypassAuction_DispatchesToSingleAgent(t *testing.T) {
	fake := &fakeScoutAuctioneer{
		resp: &domain.Handoff{
			FromAgent: "",
			Payload:   &domain.Payload{Data: []byte("the react answer")},
		},
	}
	s := &Server{
		Auctioneer: fake,
		ExecCfg: config.ExecutionConfig{
			BypassAuction: true,
			SingleAgentID: "react_baseline_agent",
		},
	}

	resp, err := s.Execute(context.Background(), &pb.Handoff{
		Payload: &pb.Object{Type: "task", Data: []byte("/plan inspect the fixtures")},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if fake.calledAgent != "react_baseline_agent" {
		t.Fatalf("dispatched to %q, want react_baseline_agent", fake.calledAgent)
	}
	got := string(fake.gotHandoff.Payload.Data)
	if got != "inspect the fixtures" {
		t.Fatalf("agent input %q, want the /plan prefix stripped", got)
	}
	if fake.gotHandoff.Context["task_id"] != "task-0" {
		t.Fatalf("task_id = %q, want task-0", fake.gotHandoff.Context["task_id"])
	}
	if string(resp.Payload.Data) != "the react answer" {
		t.Fatalf("response payload %q, want the agent's answer", resp.Payload.Data)
	}
	if resp.FromAgent != "react_baseline_agent" {
		t.Fatalf("FromAgent = %q, want the dispatched agent id", resp.FromAgent)
	}
}

func TestExecute_BypassAuction_RequiresSingleAgentID(t *testing.T) {
	s := &Server{
		Auctioneer: &fakeScoutAuctioneer{},
		ExecCfg:    config.ExecutionConfig{BypassAuction: true},
	}
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Payload: &pb.Object{Data: []byte("anything")},
	})
	if err == nil {
		t.Fatal("expected an error when single_agent_id is empty")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "single_agent_id") {
		t.Fatalf("error %q should name the missing config field", err)
	}
}
