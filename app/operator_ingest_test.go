package app

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// A PDF upload must reach the docling_agent's Docling backend. That requires BOTH
// Data (the agent gates on `bool(data_b64)`) and a SourceType the agent's
// _BINARY_TYPES set knows — either alone leaves the backend unreachable.
func TestOperatorIngestDoc_BinaryUploadRoutesToDocling(t *testing.T) {
	bytes := []byte("%PDF-1.7 fake")
	doc := operatorIngestDoc(operator.IngestRequest{
		Content:  bytes,
		Filename: "2026-W29-ops-review.pdf",
		Author:   "alice",
	})

	if doc.SourceType != "pdf" {
		t.Errorf("SourceType: want %q (docling _BINARY_TYPES gate), got %q", "pdf", doc.SourceType)
	}
	if string(doc.Data) != string(bytes) {
		t.Errorf("Data: want the original bytes, got %q", doc.Data)
	}
	// chunker_registry routes on docExt(SourceURI); no extension = unroutable.
	if !strings.HasSuffix(doc.SourceURI, ".pdf") {
		t.Errorf("SourceURI %q must keep the extension for chunker_registry ext routing", doc.SourceURI)
	}
	if doc.Title != "2026-W29-ops-review.pdf" {
		t.Errorf("Title: want the filename, got %q", doc.Title)
	}
}

// The text lane must behave exactly as it did before the binary lane existed —
// SourceType "operator_ingest", no Data. This is the regression guard.
func TestOperatorIngestDoc_TextLaneUnchanged(t *testing.T) {
	doc := operatorIngestDoc(operator.IngestRequest{
		Text:   "the sprint slipped a week",
		Source: "standup",
	})

	if doc.SourceType != "operator_ingest" {
		t.Errorf("SourceType: want %q, got %q", "operator_ingest", doc.SourceType)
	}
	if len(doc.Data) != 0 {
		t.Errorf("Data must stay empty on the text lane, got %d bytes", len(doc.Data))
	}
	if doc.Body != "the sprint slipped a week" {
		t.Errorf("Body: got %q", doc.Body)
	}
}

// An unknown extension must NOT claim a docling binary type — the agent would fall
// back to text anyway, and claiming "zzz" would just be a lie in the stored row.
func TestOperatorIngestDoc_UnknownExtFallsBack(t *testing.T) {
	doc := operatorIngestDoc(operator.IngestRequest{
		Text:     "notes",
		Filename: "notes.zzz",
	})
	if doc.SourceType != "operator_ingest" {
		t.Errorf("unknown ext must fall back to operator_ingest, got %q", doc.SourceType)
	}
}

// Context must land IN the body: metadata is neither chunked nor embedded, so a
// context note stored beside the document could never influence retrieval.
func TestApplyIngestContext_FoldsIntoBody(t *testing.T) {
	got := applyIngestContext("Revenue was flat.", "Q3 board pack; figures provisional")

	if !strings.Contains(got, "Q3 board pack; figures provisional") {
		t.Fatalf("context must appear in the body, got %q", got)
	}
	if !strings.Contains(got, "Revenue was flat.") {
		t.Fatalf("original body must survive, got %q", got)
	}
	if !strings.HasPrefix(got, "## Context") {
		t.Errorf("context should be a titled section so the parser reads it as one, got %q", got)
	}
}

func TestApplyIngestContext_EmptyIsNoop(t *testing.T) {
	if got := applyIngestContext("body", "   "); got != "body" {
		t.Errorf("blank context must not alter the body, got %q", got)
	}
}

// A binary upload with a context note has no text body to fold into; the note must
// still ride along as Body rather than being dropped on the floor.
func TestOperatorIngestDoc_ContextSurvivesBinaryUpload(t *testing.T) {
	doc := operatorIngestDoc(operator.IngestRequest{
		Content:  []byte("%PDF"),
		Filename: "report.pdf",
		Context:  "weekly management report",
	})
	if !strings.Contains(doc.Body, "weekly management report") {
		t.Errorf("context must survive on the binary lane, Body=%q", doc.Body)
	}
}
