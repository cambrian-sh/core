package memory

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-431: what used to reach only the kernel log is counted in Stats(), which the
// operator Snapshot reports: partially covered documents and the facts validation
// refused, by reason.
func TestFactExtractorStats_CountsPartialAndRefusals(t *testing.T) {
	sub := newMemSubstrate()
	gen := &scriptedGen{answer: func(p string) (string, error) {
		if isAssessmentPrompt(p) {
			return `{"verdicts":[]}`, nil
		}
		// One kept fact, one whose quote is not in the passage, one malformed.
		return `{"facts":[
			{"subject":"user","attribute":"city","value":"lisbon","passage":1,"quote":"lisbon"},
			{"subject":"user","attribute":"pet","value":"cat","passage":1,"quote":"a quote nowhere in the text"},
			{"subject":"","attribute":"x","value":"y","passage":1,"quote":"lisbon"}]}`, nil
	}}
	src := domain.SourceMeta{Title: "chat", ReferenceTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), ReferenceBasis: domain.ReferenceBasisAuthored}
	e := newTestExtractor(gen, sub, FactExtractorConfig{GeneratorModel: "m"})
	if _, err := e.ExtractDocument(context.Background(), "doc-a", "", src, factChunkDocs("I moved to lisbon last year.")); err != nil {
		t.Fatal(err)
	}
	st := e.Stats()
	if st.RefusedQuoteNotFound != 1 || st.RefusedBadShape != 1 || st.PartialDocs != 0 || st.Extracted != 1 {
		t.Fatalf("stats = %+v, want 1 quote_not_found, 1 bad_shape, 0 partial, 1 extracted", st)
	}
}
