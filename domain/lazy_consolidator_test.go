package domain_test

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Cycle 1 — ShouldConsolidate returns false when below document-count threshold.
func TestThresholdConsolidator_BelowCount_ReturnsFalse(t *testing.T) {
	c := domain.NewThresholdConsolidator(100, 0)
	metrics := domain.MemoryMetrics{TotalDocuments: 50}

	should, _ := c.ShouldConsolidate(domain.ConsolidationTriggerPressure, metrics)
	if should {
		t.Fatal("expected ShouldConsolidate=false when below threshold")
	}
}

// Cycle 2 — ShouldConsolidate returns true when document count meets threshold.
func TestThresholdConsolidator_AtCount_ReturnsTrue(t *testing.T) {
	c := domain.NewThresholdConsolidator(100, 0)
	metrics := domain.MemoryMetrics{TotalDocuments: 100}

	should, trigger := c.ShouldConsolidate(domain.ConsolidationTriggerPressure, metrics)
	if !should {
		t.Fatal("expected ShouldConsolidate=true when at threshold")
	}
	if trigger != domain.ConsolidationTriggerPressure {
		t.Errorf("expected trigger %q, got %q", domain.ConsolidationTriggerPressure, trigger)
	}
}

// Cycle 3 — ShouldConsolidate returns true when index size exceeds byte threshold.
func TestThresholdConsolidator_IndexSize_ReturnsTrue(t *testing.T) {
	const maxBytes = 1024 * 1024 // 1 MB
	c := domain.NewThresholdConsolidator(0, maxBytes)
	metrics := domain.MemoryMetrics{IndexSizeBytes: maxBytes + 1}

	should, _ := c.ShouldConsolidate(domain.ConsolidationTriggerPressure, metrics)
	if !should {
		t.Fatal("expected ShouldConsolidate=true when index size exceeds threshold")
	}
}

// Cycle 4 — Zero thresholds never trigger (consolidation disabled).
func TestThresholdConsolidator_ZeroThresholds_NeverTriggers(t *testing.T) {
	c := domain.NewThresholdConsolidator(0, 0)
	metrics := domain.MemoryMetrics{TotalDocuments: 99999, IndexSizeBytes: 9999999}

	should, _ := c.ShouldConsolidate(domain.ConsolidationTriggerPressure, metrics)
	if should {
		t.Fatal("expected ShouldConsolidate=false when both thresholds are zero")
	}
}

// Cycle 5 — Explicit trigger always returns true.
func TestThresholdConsolidator_ExplicitTrigger_AlwaysTrue(t *testing.T) {
	c := domain.NewThresholdConsolidator(100, 0)
	// Empty metrics — below count threshold.
	metrics := domain.MemoryMetrics{}

	should, trigger := c.ShouldConsolidate(domain.ConsolidationTriggerExplicit, metrics)
	if !should {
		t.Fatal("expected ShouldConsolidate=true for explicit trigger regardless of metrics")
	}
	if trigger != domain.ConsolidationTriggerExplicit {
		t.Errorf("expected trigger %q, got %q", domain.ConsolidationTriggerExplicit, trigger)
	}
}

// Cycle 6 — MemoryMetrics is correctly populated from field values.
func TestMemoryMetrics_Fields(t *testing.T) {
	m := domain.MemoryMetrics{
		TotalDocuments:    500,
		IndexSizeBytes:    1024,
		OrphanedDocuments: 10,
		StaleDocuments:    5,
		AvgQueryLatencyMs: 12.5,
		LastConsolidationAt: time.Now(),
	}
	if m.TotalDocuments != 500 {
		t.Errorf("TotalDocuments: got %d", m.TotalDocuments)
	}
}
