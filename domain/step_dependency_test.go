package domain

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fair share: a small result keeps whole, large ones are shortened to the share
// (beginning + end) on rune boundaries and marked, nothing is cut silently.
func TestFitStepDependencies_FairShare(t *testing.T) {
	deps := []StepDependency{
		{Step: 0, Content: strings.Repeat("a", 5000)},
		{Step: 1, Content: "short"},
		{Step: 2, Content: strings.Repeat("é", 3000)},
	}
	got := FitStepDependencies(deps, DependencyFit{BudgetBytes: 1000})
	if got[1].Content != "short" || got[1].Shortened {
		t.Fatalf("small result changed: %+v", got[1])
	}
	for _, i := range []int{0, 2} {
		d := got[i]
		if !d.Shortened || len(d.Content)+len(d.Tail) > 497 || len(d.Content)+len(d.Tail) < 490 {
			t.Fatalf("dep %d: share = (1000-5)/2 = 497, got head %d + tail %d", i, len(d.Content), len(d.Tail))
		}
		if !utf8.ValidString(d.Content) || !utf8.ValidString(d.Tail) {
			t.Fatalf("dep %d: a rune was split", i)
		}
	}
}

// T-20261112-summarise-a-document-step-gets-only-the-first: a head-only cut lost
// the end of p12's report (its "Leipzig" finding). The end is kept now, with the
// omitted range, the outline and the exact call for the rest.
func TestFitStepDependencies_KeepsHeadAndTail(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Report\n\nIntro.\n")
	for i := 0; i < 300; i++ {
		if i%60 == 0 {
			b.WriteString("\n## Section " + strings.Repeat("I", i/60+1) + "\n")
		}
		b.WriteString("Body line with filler text to make the report long enough to page.\n")
	}
	b.WriteString("\n## Conclusion\nThe pilot city is Leipzig.\n")
	full := b.String()
	deps := []StepDependency{{Step: 0, Content: full, Bytes: len(full), CID: "led_abc"}}
	got := FitStepDependencies(deps, DependencyFit{BudgetBytes: 4000, PageBytes: 4000})[0]
	if !got.Shortened {
		t.Fatal("not shortened")
	}
	if !strings.HasPrefix(full, got.Content) || !strings.HasSuffix(full, got.Tail) {
		t.Fatal("Content must be the beginning and Tail the end of the result")
	}
	if !strings.Contains(got.Tail, "Leipzig") {
		t.Fatalf("the end of the document was lost: tail = %q", got.Tail)
	}
	if got.OmittedFrom != len(got.Content) || got.OmittedTo != len(full)-len(got.Tail) {
		t.Fatalf("omitted range %d..%d does not match head %d / tail %d", got.OmittedFrom, got.OmittedTo, len(got.Content), len(got.Tail))
	}
	if len(got.Content)+len(got.Tail)+len(got.Outline)*8 > 4000 {
		t.Fatal("over the budget")
	}
	if len(got.Outline) == 0 || got.Outline[0].Heading != "# Report" || got.Outline[0].Page != 1 {
		t.Fatalf("outline = %+v", got.Outline)
	}
	last := got.Outline[len(got.Outline)-1]
	if last.Heading != "## Conclusion" || last.Page < 2 {
		t.Fatalf("the outline must reach the end with its page: %+v", last)
	}
	if !strings.HasPrefix(got.Read, "ledger_get id='led_abc' page=") {
		t.Fatalf("read = %q, want the exact ledger_get call", got.Read)
	}
}

func TestFitStepDependencies_RuneSafeTail(t *testing.T) {
	full := strings.Repeat("ğüşiöç€", 2000)
	got := FitStepDependencies([]StepDependency{{Content: full}}, DependencyFit{BudgetBytes: 1001})[0]
	if !utf8.ValidString(got.Content) || !utf8.ValidString(got.Tail) {
		t.Fatal("head or tail split a rune")
	}
	if got.Tail == "" || !strings.HasSuffix(full, got.Tail) {
		t.Fatal("tail is not the end of the result")
	}
}

func TestTextPageStarts_RuneSafeAndConsistent(t *testing.T) {
	text := strings.Repeat("aé", 100) // 300 bytes
	starts := TextPageStarts(text, 7)
	for _, s := range starts {
		if !utf8.RuneStart(text[s]) {
			t.Fatalf("page starts inside a rune at %d", s)
		}
	}
	if PageOfOffset(starts, 0) != 1 || PageOfOffset(starts, len(text)-1) != len(starts) {
		t.Fatal("PageOfOffset disagrees with the page starts")
	}
}

func TestFitStepDependencies_UnderBudgetAndUnbounded(t *testing.T) {
	in := []StepDependency{{Content: "x"}, {Content: "y"}}
	if got := FitStepDependencies(in, DependencyFit{BudgetBytes: 10}); got[0].Shortened || got[1].Content != "y" {
		t.Fatalf("under budget changed: %+v", got)
	}
	big := []StepDependency{{Content: strings.Repeat("z", 100)}}
	if got := FitStepDependencies(big, DependencyFit{}); got[0].Shortened || len(got[0].Content) != 100 {
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
	if err != nil || len(out) != 1 || !reflect.DeepEqual(out[0], in[0]) {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if d, err := DecodeStepDependencies(""); d != nil || err != nil {
		t.Fatal("empty must decode to nil")
	}
}
