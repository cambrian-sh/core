package config_test

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0134 P2: execution.retrieval.hnsw_iterative_scan is validated at load, read by
// the pgvector adapter, and part of the retrieval fingerprint (it changes which
// candidates a filtered dense scan can reach).
func TestHnswIterativeScan_ValidatedAndFingerprinted(t *testing.T) {
	t.Parallel()
	base := config.DefaultConfig().Execution
	// Owner decision 2026-11-02: on by default, because every principal read is
	// scope-filtered and would otherwise underfill on a large shared store.
	if base.Retrieval.HnswIterativeScan != "relaxed_order" {
		t.Fatalf("default must be relaxed_order, got %q", base.Retrieval.HnswIterativeScan)
	}
	for _, ok := range []string{"", "off", "relaxed_order", "strict_order"} {
		c := base
		c.Retrieval.HnswIterativeScan = ok
		if err := c.Validate(); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	bad := base
	bad.Retrieval.HnswIterativeScan = "relaxed"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "hnsw_iterative_scan") {
		t.Errorf("an unknown mode must be refused, got %v", err)
	}
	neg := base
	neg.Retrieval.HnswMaxScanTuples = -1
	if err := neg.Validate(); err == nil {
		t.Error("a negative hnsw_max_scan_tuples must be refused")
	}

	off, empty, on := base, base, base
	off.Retrieval.HnswIterativeScan = "off"
	empty.Retrieval.HnswIterativeScan = "" // explicit: the shipped default is now relaxed_order
	on.Retrieval.HnswIterativeScan = "relaxed_order"
	if off.RetrievalFingerprint("e") != empty.RetrievalFingerprint("e") {
		t.Error(`"" and "off" are the same ranking function and must fingerprint alike`)
	}
	if on.RetrievalFingerprint("e") == off.RetrievalFingerprint("e") {
		t.Error("turning the iterative scan on must change the fingerprint")
	}
}

// ADR-0134 D5a ships default-off: chronological answer context is turned on only
// with a recorded A/B.
func TestChronologicalContext_DefaultOff(t *testing.T) {
	t.Parallel()
	if config.DefaultConfig().Execution.Retrieval.ChronologicalContextEnabled {
		t.Fatal("execution.retrieval.chronological_context_enabled must default to false")
	}
}
