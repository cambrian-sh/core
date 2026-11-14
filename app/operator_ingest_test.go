package app

import (
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
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

// ADR-0134 (contract 0099): a multi-part upload becomes a text-lane document whose
// body is the assembled parts, dated by the resolved event time; a context note is
// prepended, and the part offsets move with it so they still point at each part.
func TestOperatorIngestDoc_PartsCarryEventTimeAndShiftedOffsets(t *testing.T) {
	a := time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC)
	et, err := domain.ResolveIngestEventTime(nil, []domain.MemoryPartInput{
		{Text: "user: hi", OccurredAt: &a},
		{Text: "assistant: hello"},
	}, "minute", time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	doc := operatorIngestDoc(operator.IngestRequest{Context: "a chat export", EventTime: et})
	if !strings.HasSuffix(doc.Body, "user: hi\n\nassistant: hello") || !strings.HasPrefix(doc.Body, "## Context") {
		t.Fatalf("body = %q", doc.Body)
	}
	if !doc.Timestamp.Equal(a) || doc.TimeSource != domain.TimeSourceExplicit || doc.TimePrecision != domain.TimePrecisionMinute {
		t.Fatalf("doc time = %v %q %q", doc.Timestamp, doc.TimeSource, doc.TimePrecision)
	}
	for i, want := range []string{"user: hi", "assistant: hello"} {
		p := doc.Parts[i]
		if got := doc.Body[p.Offset : p.Offset+p.Length]; got != want {
			t.Fatalf("part %d points at %q, want %q", i, got, want)
		}
	}
}

// A request that never went through the handler is still dated, and labelled.
func TestOperatorIngestDoc_ZeroEventTimeIsIngestDefault(t *testing.T) {
	doc := operatorIngestDoc(operator.IngestRequest{Text: "x"})
	if doc.Timestamp.IsZero() || doc.TimeSource != domain.TimeSourceIngestDefault {
		t.Fatalf("doc time = %v %q", doc.Timestamp, doc.TimeSource)
	}
}

// ADR-0138 Amendment B1: an upload carries its file name as UploadName (the name key);
// a pasted text with no filename does not, so it is never keyed or deduplicated by name.
func TestOperatorIngestDoc_UploadNameIsTheFilename(t *testing.T) {
	up := operatorIngestDoc(operator.IngestRequest{Content: []byte("%PDF"), Filename: "Report (2).pdf", Author: "alice", Tags: []string{"finance"}})
	if up.UploadName != "Report (2).pdf" || up.Author != "alice" {
		t.Errorf("upload: UploadName %q Author %q", up.UploadName, up.Author)
	}
	paste := operatorIngestDoc(operator.IngestRequest{Text: "a note", Author: "alice"})
	if paste.UploadName != "" {
		t.Errorf("paste: UploadName %q", paste.UploadName)
	}
}

// Contract 0104 (ADR-0136 A3.5): the operator's descriptive fields reach the
// document — an explicit title replaces the filename-derived one, source_kind is
// descriptive only (SourceType stays kernel-derived), author_label never replaces Author.
func TestOperatorIngestDoc_DescriptiveSourceMetadata(t *testing.T) {
	doc := operatorIngestDoc(operator.IngestRequest{Text: "body", Filename: "report.pdf", Author: "alice",
		Title: "Q3 board pack", SourceKind: "report", AuthorLabel: "Finance"})
	if doc.Title != "Q3 board pack" || doc.SourceKind != "report" || doc.AuthorLabel != "Finance" || doc.Author != "alice" {
		t.Fatalf("doc = title %q kind %q label %q author %q", doc.Title, doc.SourceKind, doc.AuthorLabel, doc.Author)
	}
	if doc.SourceType == "report" {
		t.Fatal("source_kind must never become the routing SourceType")
	}
	if got := operatorIngestDoc(operator.IngestRequest{Text: "body", Filename: "report.pdf"}).Title; got != "report.pdf" {
		t.Fatalf("no title keeps the filename: %q", got)
	}
}
