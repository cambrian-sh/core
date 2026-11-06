package aggregator

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func runOne(t *testing.T, events []domain.TaskEvent) domain.AgentProfile {
	t.Helper()
	reader := &mockTaskEventReader{events: map[string][]domain.TaskEvent{"a:h": events}, agentKeys: []string{"a:h"}}
	store := &mockAggregatorStore{}
	agg := New(reader, store, AggregatorConfig{EWMAAlpha: 0.3, LatencyWindow: 10})
	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("saved %d profiles, want 1", len(store.savedProfiles))
	}
	return store.savedProfiles[0].Profile
}

// ADR-0139 D6: the posterior evidence moves only on VERIFIED outcomes (fractionally,
// by verifier score); unverified completions are counted separately and are neutral
// unless merit_unverified_weight says otherwise (applied at scoring time).
func TestOutcomeEvidence_VerifiedOnly(t *testing.T) {
	p := runOne(t, []domain.TaskEvent{
		{TaskID: "1", Capability: "code"},
		{TaskID: "2", Capability: "code"},
		{TaskID: "3", Capability: "code", Verified: true, VerifierScore: 1},
		{TaskID: "4", Capability: "code", Verified: true, VerifierScore: 0.25},
		{TaskID: "5", Capability: "", Verified: true, VerifierScore: 0},
		{TaskID: "6", Capability: "pdf"}, // unverified only
	})
	if want := (domain.OutcomeEvidence{VerifiedSuccess: 1.25, VerifiedFailure: 1.75, Unverified: 3}); p.Outcomes != want {
		t.Errorf("global evidence = %+v, want %+v", p.Outcomes, want)
	}
	code := p.CapabilityStats["code"]
	if want := (domain.OutcomeEvidence{VerifiedSuccess: 1.25, VerifiedFailure: 0.75, Unverified: 2}); code.Outcomes != want {
		t.Errorf("code evidence = %+v, want %+v", code.Outcomes, want)
	}
	if code.SampleCount != 2 {
		t.Errorf("code SampleCount = %d, want 2 (verified only, legacy semantics)", code.SampleCount)
	}
	// An unverified-only capability gets an entry the legacy scorer skips (SampleCount 0).
	if pdf := p.CapabilityStats["pdf"]; pdf.SampleCount != 0 || pdf.Outcomes != (domain.OutcomeEvidence{Unverified: 1}) {
		t.Errorf("pdf stat = %+v, want SampleCount 0 and {Unverified:1}", pdf)
	}
}

// T-20261104-step-taskevents-carry-no-latency-so-the-merit: the recorded step
// latency is aggregated; a 0 (unknown) latency is left out of the median rather
// than dragging it to 0, and a profile with no known sample stays at 0 (unknown).
func TestLatencyMedian_IgnoresUnknown(t *testing.T) {
	p := runOne(t, []domain.TaskEvent{
		{TaskID: "1", ComputationLatencyMs: 100},
		{TaskID: "2"}, // thought step / unstamped: unknown
		{TaskID: "3", ComputationLatencyMs: 300},
		{TaskID: "4"},
		{TaskID: "5"},
	})
	if p.ComputationLatencyMedianMs != 200 || p.NetworkLatencyMedianMs != 0 {
		t.Errorf("latency medians = net %d / comp %d, want 0 / 200", p.NetworkLatencyMedianMs, p.ComputationLatencyMedianMs)
	}
	if p := runOne(t, []domain.TaskEvent{{TaskID: "1"}, {TaskID: "2"}}); p.ComputationLatencyMedianMs != 0 {
		t.Errorf("no known latency ⇒ median %d, want 0 (unknown)", p.ComputationLatencyMedianMs)
	}
}
