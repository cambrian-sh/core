package config

import "testing"

// Evidence capture defaults ON (owner decision 2026-08-11, promoting the
// operating config to shipped defaults). The original ADR-0105 D6 rationale for
// defaulting OFF — "a monotonically growing, never-consumed evidence_outbox" —
// expired when ADR-0108 gave the outbox its consumer and the substrate went
// default-on: content-first evidence is now the product's ingest path, not an
// experiment a fresh install must discover.
func TestDefaultConfig_EvidenceCaptureEnabled(t *testing.T) {
	if !DefaultConfig().Execution.Ingestion.EvidenceCaptureEnabled {
		t.Fatal("evidence_capture_enabled must default ON (owner decision 2026-08-11; ADR-0108 supplied the consumer ADR-0105 D6 was waiting on)")
	}
}
