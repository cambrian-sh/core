package config

import (
	"strings"
	"testing"
)

// ENG-210: the dense pool sizing is config with today's values as default;
// the fingerprint changes only when it moves; negatives are refused.
func TestVectorPool_DefaultsFingerprintAndValidation(t *testing.T) {
	d := DefaultConfig().Execution
	if m, mx := d.Retrieval.EffectiveVectorPool(); m != 20 || mx != 3000 {
		t.Fatalf("defaults: %d/%d", m, mx)
	}
	zero := d
	zero.Retrieval.VectorPoolMultiplier, zero.Retrieval.VectorPoolMax = 0, 0
	if zero.RetrievalFingerprint("e") != d.RetrievalFingerprint("e") {
		t.Error("0 (= default) must not change the fingerprint")
	}
	moved := d
	moved.Retrieval.VectorPoolMultiplier = 40
	if moved.RetrievalFingerprint("e") == d.RetrievalFingerprint("e") {
		t.Error("a different pool is a different ranking function: the fingerprint must change")
	}
	bad := d
	bad.Retrieval.VectorPoolMultiplier = -1
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "vector_pool_multiplier") {
		t.Fatalf("validate: %v", err)
	}
}
