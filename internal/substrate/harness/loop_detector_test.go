package harness

import "testing"

// ============================================================
// Cycle 1 — Tier 1: different error messages → false
// ============================================================

func TestDetect_DifferentErrors_ReturnsFalse(t *testing.T) {
	prev := Attempt{ErrorMsg: "timeout", Output: []byte("some output")}
	curr := Attempt{ErrorMsg: "oom", Output: []byte("some output")}
	if got := Detect(prev, curr); got != false {
		t.Errorf("Detect with different error msgs: got %v, want false", got)
	}
}

// ============================================================
// Cycle 2 — Tier 2: same error + empty output → true
// ============================================================

func TestDetect_SameError_EmptyOutput_ReturnsTrue(t *testing.T) {
	prev := Attempt{ErrorMsg: "timeout", Output: []byte("previous output")}
	curr := Attempt{ErrorMsg: "timeout", Output: []byte{}}
	if got := Detect(prev, curr); got != true {
		t.Errorf("Detect with same error and empty curr output: got %v, want true", got)
	}
}

// ============================================================
// Cycle 3 — Tier 2: same error + output > 8192 bytes → true
// ============================================================

func TestDetect_SameError_LargeOutput_ReturnsTrue(t *testing.T) {
	large := make([]byte, 8193)
	for i := range large {
		large[i] = 'x'
	}
	prev := Attempt{ErrorMsg: "crash", Output: large}
	curr := Attempt{ErrorMsg: "crash", Output: large}
	if got := Detect(prev, curr); got != true {
		t.Errorf("Detect with same error and output > 8192 bytes: got %v, want true", got)
	}
}

// ============================================================
// Cycle 4 — Tier 2: same error + output ≤ 8192 + delta < 0.05 → true
// ============================================================

func TestDetect_SameError_NearlyIdenticalOutput_ReturnsTrue(t *testing.T) {
	// 100-byte base; change 1 byte → edit distance = 1, delta = 1/100 = 0.01 < 0.05
	base := make([]byte, 100)
	for i := range base {
		base[i] = 'a'
	}
	modified := make([]byte, 100)
	copy(modified, base)
	modified[50] = 'b'

	prev := Attempt{ErrorMsg: "err", Output: base}
	curr := Attempt{ErrorMsg: "err", Output: modified}
	if got := Detect(prev, curr); got != true {
		t.Errorf("Detect with nearly identical output (delta < 0.05): got %v, want true", got)
	}
}

// ============================================================
// Cycle 5 — Tier 2: same error + output ≤ 8192 + delta ≥ 0.05 → false
// ============================================================

func TestDetect_SameError_MeaningfullyDifferentOutput_ReturnsFalse(t *testing.T) {
	// 100-byte outputs, 10 bytes different → delta = 10/100 = 0.10 ≥ 0.05
	prev := make([]byte, 100)
	curr := make([]byte, 100)
	for i := range prev {
		prev[i] = 'a'
		curr[i] = 'a'
	}
	for i := range 10 {
		curr[i] = 'z'
	}

	p := Attempt{ErrorMsg: "err", Output: prev}
	c := Attempt{ErrorMsg: "err", Output: curr}
	if got := Detect(p, c); got != false {
		t.Errorf("Detect with meaningfully different output (delta >= 0.05): got %v, want false", got)
	}
}

// ============================================================
// Cycle 6 — boundary: payload at exactly 8192 bytes uses Levenshtein
// ============================================================

func TestDetect_SameError_ExactBoundary8192_UsesLevenshtein(t *testing.T) {
	// Exactly 8192 bytes is within the Levenshtein window (≤ 8192 inclusive).
	// Identical payloads → edit distance = 0 → delta = 0 < 0.05 → true (loop confirmed).
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = 'a'
	}
	prev := Attempt{ErrorMsg: "err", Output: payload}
	curr := Attempt{ErrorMsg: "err", Output: payload}
	if got := Detect(prev, curr); got != true {
		t.Errorf("Detect at exactly 8192-byte boundary with identical output: got %v, want true", got)
	}

	// 8192 bytes with meaningful difference → false (Tier 2 runs, not short-circuited).
	different := make([]byte, 8192)
	copy(different, payload)
	// Change 500 bytes: delta = 500/8192 ≈ 0.061 ≥ 0.05 → false
	for i := range 500 {
		different[i] = 'z'
	}
	curr2 := Attempt{ErrorMsg: "err", Output: different}
	if got := Detect(prev, curr2); got != false {
		t.Errorf("Detect at exactly 8192-byte boundary with different output: got %v, want false", got)
	}
}
