package executer

import "testing"

// Test 1: ContextGrowthPenalty(0, 2.0) == 0.0
func TestContextGrowthPenalty_ZeroGrowth(t *testing.T) {
	got := ContextGrowthPenalty(0, 2.0)
	if got != 0.0 {
		t.Errorf("expected 0.0, got %v", got)
	}
}

// Test 2: ContextGrowthPenalty(1000, 0.001) == 1.0
func TestContextGrowthPenalty_KnownGrowthAndK(t *testing.T) {
	got := ContextGrowthPenalty(1000, 0.001)
	if got != 1.0 {
		t.Errorf("expected 1.0, got %v", got)
	}
}

// Test 3: ContextGrowthPenalty(500, 0.002) == 1.0
func TestContextGrowthPenalty_AlternativeKAndGrowth(t *testing.T) {
	got := ContextGrowthPenalty(500, 0.002)
	if got != 1.0 {
		t.Errorf("expected 1.0, got %v", got)
	}
}

// Test 4: contextByteSize(nil) == 0
func TestContextByteSize_NilMap(t *testing.T) {
	got := contextByteSize(nil)
	if got != 0 {
		t.Errorf("expected 0 for nil map, got %d", got)
	}
}

// Test 5: contextByteSize({"key": "val"}) == 3+3 == 6
func TestContextByteSize_SingleEntry(t *testing.T) {
	got := contextByteSize(map[string]string{"key": "val"})
	// len("key") == 3, len("val") == 3 → 6
	if got != 6 {
		t.Errorf("expected 6, got %d", got)
	}
}

// Test 6: contextByteSize with multiple entries sums correctly.
// {"a": "bb", "ccc": "dddd"} → (1+2) + (3+4) == 10
func TestContextByteSize_MultipleEntries(t *testing.T) {
	m := map[string]string{
		"a":   "bb",
		"ccc": "dddd",
	}
	got := contextByteSize(m)
	// len("a")+len("bb") == 1+2 == 3
	// len("ccc")+len("dddd") == 3+4 == 7
	// total == 10
	if got != 10 {
		t.Errorf("expected 10, got %d", got)
	}
}
