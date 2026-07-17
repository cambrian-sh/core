package routescorer

import (
	"bytes"
	"math"
	"testing"
)

// synthSamples builds a separable set where success is driven by success_rate (feature 0):
// high success_rate ⇒ label 1, low ⇒ label 0, with the other features as noise.
func synthSamples(n int) []Sample {
	out := make([]Sample, n)
	for i := 0; i < n; i++ {
		sr := float64(i%100) / 100.0 // 0..0.99 sweep
		label := 0.0
		if sr > 0.5 {
			label = 1.0
		}
		out[i] = Sample{
			Features: [NumFeatures]float64{sr, 0.5, 1.0, 0.1, 0},
			Label:    label,
		}
	}
	return out
}

func TestFit_LearnsSeparableSignal(t *testing.T) {
	train, test := Split(synthSamples(400), 0.25)
	m := Fit(train, FitOptions{})
	if m == nil {
		t.Fatal("Fit returned nil on non-empty data")
	}
	scores := make([]float64, len(test))
	labels := make([]float64, len(test))
	for i, s := range test {
		scores[i] = m.Score(s.Features)
		labels[i] = s.Label
	}
	if auc := AUC(scores, labels); auc < 0.95 {
		t.Fatalf("learned model AUC=%.3f on a separable signal, want >=0.95", auc)
	}
	// The success_rate weight should be the dominant positive coefficient.
	if m.Weights[0] <= 0 {
		t.Errorf("expected positive success_rate weight, got %.3f", m.Weights[0])
	}
}

func TestFit_EmptyIsNil(t *testing.T) {
	if Fit(nil, FitOptions{}) != nil {
		t.Fatal("Fit(nil) must return nil so the caller keeps hand weights")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	m := Fit(synthSamples(200), FitOptions{})
	var buf bytes.Buffer
	if err := m.Save(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := Load(&buf)
	if err != nil {
		t.Fatal(err)
	}
	x := [NumFeatures]float64{0.8, 0.5, 1, 0.1, 0}
	if math.Abs(got.Score(x)-m.Score(x)) > 1e-9 {
		t.Fatal("round-tripped model scores differently")
	}
}

func TestLoad_RejectsSchemaDrift(t *testing.T) {
	bad := `{"weights":[0,0,0,0,0],"bias":0,"features":["a","b","c","d","e"],"mean":[0,0,0,0,0],"std":[1,1,1,1,1],"n":1}`
	if _, err := Load(bytes.NewBufferString(bad)); err == nil {
		t.Fatal("expected schema-drift rejection")
	}
}

// The gate: on a signal the hand weights ignore (they weight only cost), the learned model
// wins; CompareOnHeldout should recommend adoption.
func TestCompareOnHeldout_GateOnRealSignal(t *testing.T) {
	train, test := Split(synthSamples(400), 0.25)
	m := Fit(train, FitOptions{})
	// A hand weight that ignores success_rate (only success_rate carries the synthetic
	// signal; the other features are constant), so the baseline is uninformative and the
	// learned model wins.
	hw := HandWeights{W1: 0, W2: 0}
	cmp := CompareOnHeldout(m, hw, test, 0.01)
	if !cmp.AdoptLearned {
		t.Fatalf("learned should beat a signal-blind baseline: learnedAUC=%.3f handAUC=%.3f", cmp.LearnedAUC, cmp.HandAUC)
	}
}

// AUC of a perfect ranker is 1.0; of an anti-ranker, 0.0; of chance, ~0.5.
func TestAUC_Sanity(t *testing.T) {
	if got := AUC([]float64{0.1, 0.9}, []float64{0, 1}); got != 1.0 {
		t.Errorf("perfect AUC=%.3f, want 1.0", got)
	}
	if got := AUC([]float64{0.9, 0.1}, []float64{0, 1}); got != 0.0 {
		t.Errorf("anti AUC=%.3f, want 0.0", got)
	}
	if got := AUC([]float64{0.5, 0.5}, []float64{1, 1}); got != -1 {
		t.Errorf("single-class AUC should be -1, got %.3f", got)
	}
}
