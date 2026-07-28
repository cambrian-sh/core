package memory

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestExternalDocumentID_ExplicitTagPreserved(t *testing.T) {
	// document upload path (document-qa): source_document tag wins, so the
	// "<doc_id>-chunk-N" evidence contract is preserved.
	doc := domain.ExternalDocument{
		SourceURI: "analyst_agent",
		Tags:      []string{"document-qa", "source_document", "tidebound-archive"},
		Body:      "Chapter 1, scene 1: ...",
	}
	if got := externalDocumentID(doc); got != "tidebound-archive" {
		t.Fatalf("explicit source_document tag must win; got %q", got)
	}
}

func TestExternalDocumentID_ThreadedTurnsAreUniqueAndStable(t *testing.T) {
	// Two conversation turns sharing one SourceURI + ThreadID (the locomo bug):
	// they must get DIFFERENT ids (no overwrite), and re-ingesting the same turn
	// must be STABLE (same id).
	turn := func(body string) domain.ExternalDocument {
		return domain.ExternalDocument{SourceURI: "analyst_agent", ThreadID: "conv-26-s9", Body: body}
	}
	a1 := externalDocumentID(turn("[date] Caroline: hi"))
	a2 := externalDocumentID(turn("[date] Caroline: hi")) // re-ingest same turn
	b := externalDocumentID(turn("[date] Mel: different turn"))

	if a1 != a2 {
		t.Fatalf("same turn must be STABLE: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("distinct turns must be UNIQUE; both got %q (the overwrite bug)", a1)
	}
	// chunk ids derived from these must also differ (the actual collision site).
	if externalChunkID(a1, 0) == externalChunkID(b, 0) {
		t.Fatalf("distinct turns produced the same chunk id")
	}
}

func TestExternalDocumentID_FileKeepsSourceURI(t *testing.T) {
	// A watched file (SourceURI, no ThreadID) keeps its path as the id so a
	// re-ingest updates in place rather than orphaning chunks.
	doc := domain.ExternalDocument{SourceURI: "/data/report.pdf", Body: "v1"}
	if got := externalDocumentID(doc); got != "/data/report.pdf" {
		t.Fatalf("file id must stay the SourceURI; got %q", got)
	}
}

func TestExternalDocumentID_BareTagIsGroupingNotIdentity(t *testing.T) {
	// The regression this branch existed to cause. N documents sharing ONE
	// classification tag must get N distinct ids: a tag groups, it does not
	// identify. Before the fix all of these collapsed onto the tag itself, so
	// their chunks shared "<tag>-chunk-K" and each ingest silently overwrote the
	// previous document's chunks while still reporting success.
	tagged := func(body string) domain.ExternalDocument {
		return domain.ExternalDocument{
			SourceURI: "memory-guard-" + body[:4],
			ThreadID:  "memory-guard:v1:" + body[:4],
			Tags:      []string{"memory-guard"},
			Body:      body,
		}
	}
	a := externalDocumentID(tagged("Marrowgate Institute was established in 1971"))
	b := externalDocumentID(tagged("Vellum Harbour Trust was established in 2010"))
	c := externalDocumentID(tagged("Ostrand Survey Office was established in 1974"))

	ids := map[string]bool{a: true, b: true, c: true}
	if len(ids) != 3 {
		t.Fatalf("documents sharing a tag must get distinct ids; got %q, %q, %q", a, b, c)
	}
	for _, id := range []string{a, b, c} {
		if id == "memory-guard" {
			t.Fatalf("id collapsed onto the bare tag: %q", id)
		}
	}
}

func TestExternalDocumentID_BareTagIsStableOnReingest(t *testing.T) {
	// The other half of the contract: distinct bodies differ, but the SAME body
	// re-ingested resolves to the same id, so a re-ingest updates in place rather
	// than orphaning the old chunks.
	doc := domain.ExternalDocument{
		Tags: []string{"memory-guard"},
		Body: "Marrowgate Institute was established in 1971",
	}
	if externalDocumentID(doc) != externalDocumentID(doc) {
		t.Fatal("re-ingesting identical content must be idempotent")
	}
}

func TestExternalDocumentID_ExplicitIDStillBeatsDigest(t *testing.T) {
	// Rule 1a is untouched: a caller that explicitly names the document via
	// source_document still owns the id, digest or not. document-qa's
	// "<doc_id>-chunk-N" evidence contract depends on this.
	doc := domain.ExternalDocument{
		Tags: []string{"document-qa", "source_document", "tidebound-archive", "chunker:late"},
		Body: "anything",
	}
	if got := externalDocumentID(doc); got != "tidebound-archive" {
		t.Fatalf("explicit id must still win; got %q", got)
	}
}
