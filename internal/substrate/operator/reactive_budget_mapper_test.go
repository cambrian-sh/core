package operator

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// REACT-02 / ADR-0062: ReactiveBudgetEvent maps to the ReactiveBudgetOp feed payload.
func TestToOperatorEvent_ReactiveBudget(t *testing.T) {
	at := time.Unix(0, 1_000_000*4321).UTC()
	se := domain.SequencedEvent{
		Seq: 7,
		At:  at,
		Event: domain.ReactiveBudgetEvent{
			Resource:      "llm_condition",
			Reason:        "budget_exhausted",
			StreamID:      "s1",
			SheddingSince: at,
		},
	}
	ev := toOperatorEvent(se)
	rb := ev.GetReactiveBudget()
	if rb == nil {
		t.Fatalf("expected ReactiveBudgetOp payload, got %T", ev.GetPayload())
	}
	if rb.GetResource() != "llm_condition" || rb.GetReason() != "budget_exhausted" ||
		rb.GetStreamId() != "s1" || rb.GetSheddingSinceUnixMs() != 4321 {
		t.Fatalf("unexpected mapping: %+v", rb)
	}
}
