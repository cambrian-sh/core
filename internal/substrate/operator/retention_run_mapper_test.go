package operator

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0102 Amendment A1: RetentionRunEvent maps to the RetentionRunOp feed payload.
func TestToOperatorEvent_RetentionRun(t *testing.T) {
	start := time.Unix(1750000000, 0).UTC()
	end := start.Add(2 * time.Second)
	se := domain.SequencedEvent{
		Seq: 11,
		At:  end,
		Event: domain.RetentionRunEvent{
			Source:     "records",
			RunID:      42,
			StartedAt:  start,
			FinishedAt: end,
			Deleted: []domain.RetentionDeletion{
				{Category: "record_versions", Count: 280},
				{Category: "push_commands", Count: 3},
			},
			Bounded: true,
		},
	}

	rr := toOperatorEvent(se).GetRetentionRun()
	if rr == nil {
		t.Fatalf("expected RetentionRunOp payload, got %T", toOperatorEvent(se).GetPayload())
	}
	if rr.GetSource() != "records" || rr.GetRunId() != 42 || !rr.GetBounded() {
		t.Fatalf("unexpected mapping: %+v", rr)
	}
	if !rr.GetStartedAt().AsTime().Equal(start) || !rr.GetFinishedAt().AsTime().Equal(end) {
		t.Fatalf("timestamps not carried: started=%v finished=%v",
			rr.GetStartedAt().AsTime(), rr.GetFinishedAt().AsTime())
	}
	got := map[string]int32{}
	for _, d := range rr.GetDeleted() {
		got[d.GetCategory()] = d.GetCount()
	}
	if got["record_versions"] != 280 || got["push_commands"] != 3 {
		t.Fatalf("per-category counts not carried: %+v", got)
	}
}

// A FAILED pass is the one most worth surfacing: retention that quietly stops
// working is how a table becomes the outage. The error must survive the mapping.
func TestToOperatorEvent_RetentionRun_CarriesFailure(t *testing.T) {
	se := domain.SequencedEvent{
		Seq: 12,
		At:  time.Unix(1750000100, 0).UTC(),
		Event: domain.RetentionRunEvent{
			Source: "records",
			RunID:  43,
			Err:    "citation floor unreadable: connection refused",
		},
	}
	rr := toOperatorEvent(se).GetRetentionRun()
	if rr == nil {
		t.Fatalf("a failed pass produced no payload at all")
	}
	if rr.GetError() == "" {
		t.Fatalf("error dropped in mapping: %+v", rr)
	}
	if len(rr.GetDeleted()) != 0 {
		t.Fatalf("a failed pass reported deletions: %+v", rr.GetDeleted())
	}
}

// Retention is a background pass, not session work. Attributing it to whichever
// session happened to be live would invent a causal link that does not exist.
func TestToOperatorEvent_RetentionRun_HasNoSession(t *testing.T) {
	se := domain.SequencedEvent{
		Seq:   13,
		At:    time.Unix(1750000200, 0).UTC(),
		Event: domain.RetentionRunEvent{Source: "records", RunID: 44},
	}
	if got := toOperatorEvent(se).GetSessionId(); got != "" {
		t.Fatalf("retention event attributed to session %q", got)
	}
}

// THE test for this change, and the reason it exists.
//
// ROUTE-08.A shipped a ScoutUsefulnessEvent that was published to the bus and had
// a working mapper case, and it reached nobody for weeks — because the type was
// missing from feedEventTypes. Every unit test passed the whole time. A mapper
// case is necessary and not sufficient; this asserts the half that was missed.
func TestRetentionRunIsBridgedToTheFeed(t *testing.T) {
	for _, tp := range feedEventTypes {
		if tp == domain.EventTypeRetentionRun {
			return
		}
	}
	t.Fatalf("EventTypeRetentionRun has a mapper case but is not in feedEventTypes — "+
		"it will be published and silently never delivered (the ROUTE-08.A defect). Have: %v",
		feedEventTypes)
}
