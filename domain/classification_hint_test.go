package domain

import "testing"

// ADR-0099: identity is not a classification. These assert the SPLIT, not just the
// function — the premise being protected is that a document can carry its own name
// without that name being judged against a classification vocabulary.

func TestClassificationHint_StripsMarkerAndTheIdItIntroduces(t *testing.T) {
	// The exact shape six benchmark suites and the operator upload lane send.
	got := ClassificationHint([]string{"orchestration", "runbook", SourceDocumentMarker, "task_042"})
	want := []string{"orchestration", "runbook"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestClassificationHint_KeepsClassificationsAfterTheIdentityPair(t *testing.T) {
	// Regression guard: consuming the successor must not swallow the rest of the list.
	got := ClassificationHint([]string{SourceDocumentMarker, "doc_1", "finance", "internal_only"})
	if len(got) != 2 || got[0] != "finance" || got[1] != "internal_only" {
		t.Fatalf("identity pair swallowed following classifications: got %v", got)
	}
}

func TestClassificationHint_TrailingMarkerConsumesNothing(t *testing.T) {
	got := ClassificationHint([]string{"finance", SourceDocumentMarker})
	if len(got) != 1 || got[0] != "finance" {
		t.Fatalf("got %v, want [finance]", got)
	}
}

func TestClassificationHint_LeavesAPureClassificationListAlone(t *testing.T) {
	in := []string{"public", "finance"}
	got := ClassificationHint(in)
	if len(got) != 2 || got[0] != "public" || got[1] != "finance" {
		t.Fatalf("a list with no identity terms must pass through unchanged: got %v", got)
	}
}

// The test that makes the others non-vacuous: if ClassificationHint were the identity
// function, every assertion above except this one would still hold for SOME input, so
// state the property directly — no identity term survives.
func TestClassificationHint_NoIdentityTermSurvives(t *testing.T) {
	for _, tags := range [][]string{
		{SourceDocumentMarker, "id_a"},
		{"finance", SourceDocumentMarker, "id_b"},
		{SourceDocumentMarker, "id_c", "hr"},
	} {
		for _, out := range ClassificationHint(tags) {
			if out == SourceDocumentMarker {
				t.Fatalf("marker survived for %v", tags)
			}
			if out == "id_a" || out == "id_b" || out == "id_c" {
				t.Fatalf("identity term %q survived for %v", out, tags)
			}
		}
	}
}
