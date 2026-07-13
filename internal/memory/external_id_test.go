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
