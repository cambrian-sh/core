package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0150 D3: a single-agent turn carries the lane's D4 caps, binds the
// person's permitted tools to its lease, and is not shown the planner's
// work-systems list (the agent holds the tools itself).
func TestLaneTurn_StampsCapsAndComposesTheTurnLease(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.svc.SetWorkSystems(func(context.Context) string { return "crm, erp" })
	r.svc.Lane = LaneTurn{SingleAgent: true, Limits: domain.LaneLimits{MaxToolRounds: 500, MaxWallSeconds: 3600, SameResultRepeats: 4, SameFailureRepeats: 3}}
	var composed []string
	r.svc.SetTurnComposer(func(_ context.Context, lease domain.LeaseID, brief string) error {
		composed = append(composed, string(lease)+"|"+brief)
		return nil
	})
	_, h := r.turn(t, "what's overdue?")
	c := h.Context
	if c[domain.HandoffKeyLane] != domain.LaneSingleAgent || c[domain.HandoffKeyLaneMaxToolRounds] != "500" ||
		c[domain.HandoffKeyLaneMaxWallSeconds] != "3600" || c[domain.HandoffKeyLaneSameResult] != "4" ||
		c[domain.HandoffKeyLaneSameFailure] != "3" {
		t.Fatalf("turn context %v, want the lane keys", c)
	}
	if len(composed) != 1 || composed[0] != "lease-1|what's overdue?" {
		t.Fatalf("composed = %v, want the turn's lease bound once with the message", composed)
	}
	if _, shown := h.Payload.Metadata["work_systems"]; shown {
		t.Fatal("the single-agent turn was shown the planner's work-systems list")
	}
	// A failing composition leaves the turn running on the agent's own grants.
	r.svc.SetTurnComposer(func(context.Context, domain.LeaseID, string) error { return fmt.Errorf("off") })
	r.turn(t, "again")
}

// The planner lane's front desk is unchanged: no lane keys, no composition.
func TestLaneTurn_PlannerLaneIsUnchanged(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.svc.SetWorkSystems(func(context.Context) string { return "crm" })
	called := false
	r.svc.SetTurnComposer(func(context.Context, domain.LeaseID, string) error { called = true; return nil })
	_, h := r.turn(t, "hi")
	if _, has := h.Context[domain.HandoffKeyLane]; has || called {
		t.Fatal("the planner lane got single-agent keys or a composition")
	}
	if h.Payload.Metadata["work_systems"] != "crm" {
		t.Fatal("the front desk lost its work-systems list")
	}
}

// ADR-0150 D3 compaction order: older long replies are cleared first (no LLM);
// the summary runs only when that is not enough, folds the ORIGINAL messages,
// and the person's own messages survive verbatim up to the cap.
func TestLaneCompaction_ClearsOldRepliesBeforeSummarising(t *testing.T) {
	long := strings.Repeat("x", 4000) // ~1000 tokens: tool output pasted in a reply
	msgs := []domain.Message{
		{Seq: 1, Role: domain.MessageRoleUser, Content: "list the invoices"},
		{Seq: 2, Role: domain.MessageRoleAgent, Content: long},
		{Seq: 3, Role: domain.MessageRoleUser, Content: "and the overdue ones?"},
		{Seq: 4, Role: domain.MessageRoleAgent, Content: "two are overdue"},
		{Seq: 5, Role: domain.MessageRoleUser, Content: "thanks"},
		{Seq: 6, Role: domain.MessageRoleAgent, Content: "welcome"},
	}
	// Budget fits everything once the long reply is cleared: no overflow at all.
	over, win := compactLane(msgs, 200)
	if len(over) != 0 || len(win) != 6 {
		t.Fatalf("overflow %d window %d, want the pre-pass alone to fit", len(over), len(win))
	}
	if !strings.Contains(win[1].Content, "ledger_find") || len(win[1].Content) > clearReplyHead+len(clearedReplyNote) {
		t.Fatalf("the old long reply was not cleared: %d chars", len(win[1].Content))
	}
	if msgs[1].Content != long {
		t.Fatal("the pre-pass changed the stored messages")
	}
	// Planner lane: the same history overflows and goes to the summary.
	if over, _ := budgetWindow(msgs, 200); len(over) == 0 {
		t.Fatal("expected the plain window to overflow")
	}
	// Too small even after clearing: the overflow is the original wording.
	over, _ = compactLane(msgs, 20)
	if len(over) == 0 || over[len(over)-1].Seq < 2 {
		t.Fatalf("overflow %v", over)
	}
	for _, m := range over {
		if m.Seq == 2 && m.Content != long {
			t.Fatal("the summary must fold the original reply, not the cleared one")
		}
	}
}

func TestLaneCompaction_KeepsThePersonsMessagesVerbatim(t *testing.T) {
	r := newGroundingRig(t, Grounding{TranscriptBudgetTokens: 60})
	r.svc.Lane = LaneTurn{SingleAgent: true, KeepUserMessagesTokens: 20000}
	r.svc.SetSummarizer(func(_ context.Context, prev string, msgs []domain.Message) (string, error) {
		return fmt.Sprintf("Goal:\n- through %d", msgs[len(msgs)-1].Seq), nil
	})
	long := strings.Repeat("word ", 40)
	r.pool.replies = []string{long, long, long}
	r.turn(t, "my budget is 5000 EUR, never email Bob "+long)
	r.turn(t, "second")
	_, h := r.turn(t, "third")
	tr := h.Payload.Metadata["transcript"]
	if h.Payload.Metadata[turnKeySummary] == "" {
		t.Fatal("no summary: the test history must overflow")
	}
	if !strings.Contains(tr, "my budget is 5000 EUR, never email Bob") {
		t.Fatalf("the person's summarised message is not carried verbatim: %q", tr)
	}
	// With the cap at 0 it is not.
	r2 := newGroundingRig(t, Grounding{TranscriptBudgetTokens: 60})
	r2.svc.Lane = LaneTurn{SingleAgent: true}
	r2.svc.SetSummarizer(func(_ context.Context, _ string, msgs []domain.Message) (string, error) { return "s", nil })
	r2.pool.replies = []string{long, long, long}
	r2.turn(t, "my budget is 5000 EUR "+long)
	r2.turn(t, "second")
	_, h2 := r2.turn(t, "third")
	if strings.Contains(h2.Payload.Metadata["transcript"], "my budget is 5000 EUR") {
		t.Fatal("keep_user_messages_tokens 0 still carried the message")
	}
}
