package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/memory"
)

// ENG-1647: a lister failure (zero report) prints "lister failed", never a
// "candidates=0" summary that reads like a clean empty run; a real run, and a
// run that failed after listing, keep the summary line.
func TestPrintSummary_ListerFailureIsNotAnEmptyRun(t *testing.T) {
	a := summaryArgs{version: "v1", concurrency: 2, groupConcurrency: 1}
	var buf bytes.Buffer
	if printSummary(&buf, memory.FactBackfillReport{}, errors.New("dial tcp: connection refused"), a) {
		t.Fatal("the rest of the summary must not follow a lister failure")
	}
	if out := buf.String(); !strings.Contains(out, "facts-backfill: lister failed: dial tcp: connection refused") || strings.Contains(out, "candidates=") {
		t.Fatalf("output %q", out)
	}

	buf.Reset()
	if !printSummary(&buf, memory.FactBackfillReport{}, nil, a) || !strings.Contains(buf.String(), "candidates=0") {
		t.Fatalf("a clean empty run keeps its summary: %q", buf.String())
	}
	buf.Reset()
	if !printSummary(&buf, memory.FactBackfillReport{Candidates: 5, Pending: 2}, errors.New("cancelled"), a) ||
		!strings.Contains(buf.String(), "candidates=5") {
		t.Fatalf("a run that failed after listing keeps its summary: %q", buf.String())
	}
}
