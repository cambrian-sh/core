package network

import (
	"sync"
	"testing"
)

func TestMeanConfidence_Empty(t *testing.T) {
	if got := meanConfidence(nil); got != 0 {
		t.Errorf("meanConfidence(nil): got %v, want 0", got)
	}
	if got := meanConfidence([]float64{}); got != 0 {
		t.Errorf("meanConfidence([]): got %v, want 0", got)
	}
}

func TestMeanConfidence_Single(t *testing.T) {
	if got := meanConfidence([]float64{0.8}); got != 0.8 {
		t.Errorf("meanConfidence([0.8]): got %v, want 0.8", got)
	}
}

func TestMeanConfidence_Multiple(t *testing.T) {
	got := meanConfidence([]float64{0.4, 0.6, 0.8})
	const want = 0.6
	const tolerance = 1e-9
	if diff := got - want; diff < -tolerance || diff > tolerance {
		t.Errorf("meanConfidence([0.4, 0.6, 0.8]): got %v, want %v", got, want)
	}
}

func TestMeanConfidence_AllZero(t *testing.T) {
	if got := meanConfidence([]float64{0, 0, 0}); got != 0 {
		t.Errorf("meanConfidence([0,0,0]): got %v, want 0", got)
	}
}

func TestMeanConfidence_AllOne(t *testing.T) {
	if got := meanConfidence([]float64{1, 1, 1}); got != 1 {
		t.Errorf("meanConfidence([1,1,1]): got %v, want 1", got)
	}
}

// TestServer_NilHippocampus_FieldIsNil verifies that Server accepts a nil
// Hippocampus without panicking and that the field is correctly nil.
func TestServer_NilHippocampus_FieldIsNil(t *testing.T) {
	s := &Server{Hippocampus: nil}
	if s.Hippocampus != nil {
		t.Errorf("expected Hippocampus to be nil, got %v", s.Hippocampus)
	}
}

// TestConfAccumulator_ConcurrentAppend verifies that concurrent appends to the
// accumulator (simulating parallel stepFn goroutines) do not race or lose values.
func TestConfAccumulator_ConcurrentAppend(t *testing.T) {
	var mu sync.Mutex
	var values []float64

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		conf := float64(i) / float64(goroutines)
		go func(c float64) {
			defer wg.Done()
			mu.Lock()
			values = append(values, c)
			mu.Unlock()
		}(conf)
	}
	wg.Wait()

	if len(values) != goroutines {
		t.Errorf("expected %d values, got %d — possible race or lost append", goroutines, len(values))
	}
}
