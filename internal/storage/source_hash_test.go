package storage

import (
	"testing"
)

// Test 4 (spec): Empty version + empty content → stable (no panic), returns non-empty string.
// This complements bbolt_adapter_test.go tests 1–3.
// bbolt_adapter_test.go covers empty-version + non-empty content; this covers fully empty inputs.
func TestComputeSourceHash_EmptyVersionAndEmptyContent(t *testing.T) {
	h1 := ComputeSourceHash("", []byte{})
	if h1 == "" {
		t.Error("expected non-empty hash for empty version and empty content")
	}

	// Must be stable (deterministic)
	h2 := ComputeSourceHash("", []byte{})
	if h1 != h2 {
		t.Errorf("expected same hash on repeated call with empty inputs, got %q and %q", h1, h2)
	}
}
