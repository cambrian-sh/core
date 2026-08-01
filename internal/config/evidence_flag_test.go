package config

import "testing"

// ADR-0105 D6: evidence capture is opt-in until Phase 2 gives the outbox a
// consumer. A default-on here would give every deployment a monotonically
// growing, never-consumed evidence_outbox.
func TestDefaultConfig_EvidenceCaptureDisabled(t *testing.T) {
	if DefaultConfig().Execution.Ingestion.EvidenceCaptureEnabled {
		t.Fatal("evidence_capture_enabled must default OFF (ADR-0105 D6)")
	}
}
