package network

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D5a: the ask-back round and pending ref reach Execute only from the
// turn lease the chat lane bound, never from what the caller sent.
func TestStampAskBackComesFromTheLeaseNotTheCaller(t *testing.T) {
	forged := &pb.Handoff{Metadata: map[string]string{
		domain.HandoffKeyAskBackRound: "99", domain.HandoffKeyAskBackPendingRef: "led_forged",
	}}
	stampAskBack(forged, func(map[string]string) (domain.LeaseBinding, bool) { return domain.LeaseBinding{}, true })
	if _, ok := forged.Metadata[domain.HandoffKeyAskBackRound]; ok {
		t.Errorf("a caller-supplied round survived: %v", forged.Metadata)
	}
	if _, ok := forged.Metadata[domain.HandoffKeyAskBackPendingRef]; ok {
		t.Errorf("a caller-supplied pending ref survived: %v", forged.Metadata)
	}

	in := &pb.Handoff{}
	stampAskBack(in, func(map[string]string) (domain.LeaseBinding, bool) {
		return domain.LeaseBinding{AskBackRound: 2, AskBackPendingRef: "led_pending"}, true
	})
	if in.Metadata[domain.HandoffKeyAskBackRound] != "2" || in.Metadata[domain.HandoffKeyAskBackPendingRef] != "led_pending" {
		t.Errorf("stamped = %v", in.Metadata)
	}
}

// The chat lane reads a turn's direct delegations' results kernel-side, until
// the turn ends; a nested plan's result is not the turn's.
func TestTurnResultsKeepDirectRunsUntilTheTurnEnds(t *testing.T) {
	d := NewTurnDelegations()
	_, run, err := d.BeginRun(context.Background(), domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	run.Attach("run-1", nil)
	// A step agent of run-1 delegating again: the same turn's work, not its result.
	_, nested, err := d.BeginRun(context.Background(), domain.LeaseBinding{RunID: "run-1"})
	if err != nil || nested == nil {
		t.Fatalf("nested run: %v %v", nested, err)
	}
	inner, _ := json.Marshal(domain.ExecuteResult{Version: 1, RunID: "run-nested", FailedStep: -1})
	nested.SetResult(map[string]string{domain.HandoffKeyExecuteResult: string(inner)})
	nested.End()

	res, _ := json.Marshal(domain.ExecuteResult{Version: 1, RunID: "run-1", FailedStep: -1})
	run.SetResult(map[string]string{domain.HandoffKeyExecuteResult: string(res)})
	run.End()

	got := d.TurnResults("m1")
	if len(got) != 1 || got[0].RunID != "run-1" {
		t.Fatalf("turn results = %+v", got)
	}
	d.EndTurn("c1", "m1", "", nil)
	if got := d.TurnResults("m1"); len(got) != 0 {
		t.Errorf("results outlived the turn: %+v", got)
	}
}
