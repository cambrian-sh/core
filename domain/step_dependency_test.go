package domain

import (
	"strings"
	"testing"
)

// Fair share: a small result keeps whole, large ones are cut to the share on a rune
// boundary and marked, nothing is cut silently.
func TestFitStepDependencies_FairShare(t *testing.T) {
	deps := []StepDependency{
		{Step: 0, Content: strings.Repeat("a", 5000)},
		{Step: 1, Content: "short"},
		{Step: 2, Content: strings.Repeat("é", 3000)},
	}
	got := FitStepDependencies(deps, 1000)
	if got[1].Content != "short" || got[1].Shortened {
		t.Fatalf("small result changed: %+v", got[1])
	}
	if len(got[0].Content) != 497 || !got[0].Shortened {
		t.Fatalf("share = (1000-5)/2 = 497, got %d", len(got[0].Content))
	}
	if len(got[2].Content) != 496 || !got[2].Shortened {
		t.Fatalf("a 2-byte rune must not be split (497 -> 496), got %d", len(got[2].Content))
	}
}

func TestFitStepDependencies_UnderBudgetAndUnbounded(t *testing.T) {
	in := []StepDependency{{Content: "x"}, {Content: "y"}}
	if got := FitStepDependencies(in, 10); got[0].Shortened || got[1].Content != "y" {
		t.Fatalf("under budget changed: %+v", got)
	}
	big := []StepDependency{{Content: strings.Repeat("z", 100)}}
	if got := FitStepDependencies(big, 0); got[0].Shortened || len(got[0].Content) != 100 {
		t.Fatal("budget <= 0 must leave content whole")
	}
}

func TestStepDependencies_RoundTrip(t *testing.T) {
	in := []StepDependency{{Step: 3, Objective: "o", Status: StepDependencyCompleted, CID: "c", Bytes: 2, Content: "hi"}}
	s, err := EncodeStepDependencies(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeStepDependencies(s)
	if err != nil || len(out) != 1 || out[0] != in[0] {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if d, err := DecodeStepDependencies(""); d != nil || err != nil {
		t.Fatal("empty must decode to nil")
	}
}
