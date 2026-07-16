package aggregator

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// computeCapabilityStats groups verified events by capability and computes per-tag
// success/trust; untagged events are ignored.
func TestComputeCapabilityStats(t *testing.T) {
	cfg := AggregatorConfig{EWMAAlpha: 0.5}
	verified := []domain.TaskEvent{
		// browser: consistently high quality.
		{Capability: "browser", VerifierScore: 0.9, BidConfidence: 0.9, Verified: true},
		{Capability: "browser", VerifierScore: 0.8, BidConfidence: 0.9, Verified: true},
		// pdf: consistently poor.
		{Capability: "pdf", VerifierScore: 0.2, BidConfidence: 0.9, Verified: true},
		{Capability: "pdf", VerifierScore: 0.1, BidConfidence: 0.9, Verified: true},
		// untagged: must be skipped.
		{Capability: "", VerifierScore: 0.9, BidConfidence: 0.9, Verified: true},
	}
	stats := computeCapabilityStats(verified, cfg, 0.6, 0.4)
	if len(stats) != 2 {
		t.Fatalf("expected 2 capability stats (browser, pdf), got %d: %v", len(stats), stats)
	}
	br, pdf := stats["browser"], stats["pdf"]
	if br.SampleCount != 2 || pdf.SampleCount != 2 {
		t.Fatalf("sample counts wrong: browser=%d pdf=%d", br.SampleCount, pdf.SampleCount)
	}
	// browser (verifier > 0.5 both) → success 1.0; pdf (both < 0.5) → success 0.0.
	if br.SuccessRate <= pdf.SuccessRate {
		t.Fatalf("browser success (%.2f) should exceed pdf success (%.2f)", br.SuccessRate, pdf.SuccessRate)
	}
	if br.TrustScore <= pdf.TrustScore {
		t.Fatalf("browser trust (%.2f) should exceed pdf trust (%.2f)", br.TrustScore, pdf.TrustScore)
	}
}

func TestComputeCapabilityStats_NoTags(t *testing.T) {
	cfg := AggregatorConfig{EWMAAlpha: 0.5}
	if got := computeCapabilityStats([]domain.TaskEvent{{VerifierScore: 0.9}}, cfg, 0.6, 0.4); got != nil {
		t.Fatalf("no tagged events should yield nil, got %v", got)
	}
}
