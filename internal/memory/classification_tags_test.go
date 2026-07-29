package memory

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The case the whole split exists for: the ingest convention passes
// `source_document` + the document id so `externalDocumentID` can make it the id.
// That pair is IDENTITY, not classification — a tag naming exactly one document is
// a term no rule can usefully match, and one per document buries a 12-term
// vocabulary under 710 junk entries.
func TestClassificationTags_StripsTheIdentityPair(t *testing.T) {
	got := classificationTags(
		[]string{"tau2-knowledge", "source_document", "doc_savings_gold_010"},
		"doc_savings_gold_010",
	)
	if want := []string{"tau2-knowledge"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Identity is stripped from the LABELS but must still resolve the id. This is the
// regression that makes the split necessary rather than optional: dropping the tag
// at the caller instead renames every document to `<tag>:<content-digest>`, and the
// tau2-knowledge scorer — which matches `metadata["document_id"]` against the
// dataset's `required_documents` — then scores a flat 0.0 recall on every task.
func TestClassificationTags_DoesNotChangeTheResolvedDocumentID(t *testing.T) {
	doc := domain.ExternalDocument{
		Body: "gold account terms",
		Tags: []string{"tau2-knowledge", "source_document", "doc_savings_gold_010"},
	}
	id := externalDocumentID(doc)
	if id != "doc_savings_gold_010" {
		t.Fatalf("document id = %q, want the caller-supplied id", id)
	}

	// Strip, then re-resolve: the id is read BEFORE stripping in persistChunks, so
	// this only asserts the ordering contract that makes that safe.
	doc.Tags = classificationTags(doc.Tags, id)
	if reResolved := externalDocumentID(doc); reResolved == id {
		t.Fatalf("re-resolving after the strip returned the same id (%q) — the test "+
			"no longer proves the id must be captured first", id)
	}
}

// A caller whose id came from somewhere else (thread, source URI, digest) keeps
// every tag it sent. Blindly dropping whatever follows the marker would silently
// delete a real label.
func TestClassificationTags_KeepsTagsThatAreNotTheID(t *testing.T) {
	got := classificationTags(
		[]string{"finance", "source_document", "confidential"},
		"conv-26-s1:abcdef",
	)
	if want := []string{"finance", "confidential"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestClassificationTags_LeavesOrdinaryLabelsAlone(t *testing.T) {
	in := []string{"finance", "internal_only"}
	got := classificationTags(in, "doc-1")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want %v", got, in)
	}
	if len(classificationTags(nil, "doc-1")) != 0 {
		t.Fatal("nil tags should stay empty")
	}
}

// The id can appear without the marker (a caller that tagged the document with its
// own name). Still not a classification.
func TestClassificationTags_StripsABareSelfID(t *testing.T) {
	got := classificationTags([]string{"doc-1", "finance"}, "doc-1")
	if want := []string{"finance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Every suite using the convention gets the fix without changing: the id keeps
// resolving uniquely per artifact (no doc-id collision) while the label set stays
// the corpus markers only.
func TestClassificationTags_MultiSuiteConvention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tags    []string
		wantID  string
		wantTag []string
	}{
		{"agentic-retrieval", []string{"agentic-retrieval", "adr_excerpt", "source_document", "art-7"}, "art-7", []string{"agentic-retrieval", "adr_excerpt"}},
		{"orchestration", []string{"orchestration", "incident_note", "source_document", "fx-3"}, "fx-3", []string{"orchestration", "incident_note"}},
		{"musique", []string{"musique", "source_document", "para-9"}, "para-9", []string{"musique"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := domain.ExternalDocument{Body: "body", Tags: tc.tags}
			id := externalDocumentID(doc)
			if id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
			if got := classificationTags(tc.tags, id); !reflect.DeepEqual(got, tc.wantTag) {
				t.Fatalf("tags = %v, want %v", got, tc.wantTag)
			}
		})
	}
}
