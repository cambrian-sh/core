package memory

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// captureParser records the StructureParseRequest the ingest path built.
type captureParser struct {
	got  StructureParseRequest
	seen bool
}

func (c *captureParser) Parse(_ context.Context, req StructureParseRequest) (*StructuredDocument, error) {
	c.got = req
	c.seen = true
	// Returning nil keeps the ingest path on its flat-chunk fallback; this test is
	// only about what the ingest path SENDS to the sidecar.
	return nil, nil
}

type noopStructStore struct{}

func (noopStructStore) SaveSections(context.Context, []SectionRow) error          { return nil }
func (noopStructStore) StampChunks(context.Context, []ChunkStamp) error           { return nil }
func (noopStructStore) SaveStructuralEdges(context.Context, []StructuralEdge) error { return nil }

func newCaptureIngestManager(t *testing.T, p StructureParser) *IngestionManager {
	t.Helper()
	agent := &Agent{
		Manager:    &MemoryManager{Store: &captureAllStore{}, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}},
		pendingCap: 256,
	}
	reg, err := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	im := NewIngestionManagerWithRegistry(nil, &mockEmbedder{vec: []float32{0.1, 0.2}}, agent, reg, IngestionConfig{})
	im.SetStructureGraph(p, noopStructStore{})
	return im
}

// REGRESSION: the docling_agent gates its Docling backend on
// `want_docling = bool(data_b64) and (...)`. The ingest path used to build
// StructureParseRequest with only Text, so DataB64 was never set by ANY production
// caller and the Docling backend was unreachable — a PDF could not be parsed no
// matter how it was uploaded. Binary bytes must arrive base64-encoded.
func TestPersistChunks_BinaryDocumentSendsDataB64(t *testing.T) {
	p := &captureParser{}
	im := newCaptureIngestManager(t, p)

	raw := []byte("%PDF-1.7 binary bytes")
	doc := domain.ExternalDocument{
		SourceURI:  "operator_ingest://report.pdf",
		SourceType: "pdf",
		Title:      "report.pdf",
		Data:       raw,
	}

	if _, err := im.persistChunks(context.Background(), doc, nil, "entity-1", ""); err != nil {
		t.Fatalf("persistChunks: %v", err)
	}

	if !p.seen {
		t.Fatal("structure parser was never called")
	}
	want := base64.StdEncoding.EncodeToString(raw)
	if p.got.DataB64 != want {
		t.Errorf("DataB64: want the base64 of the original bytes, got %q", p.got.DataB64)
	}
	if p.got.SourceType != "pdf" {
		t.Errorf("SourceType must reach the sidecar for its _BINARY_TYPES gate, got %q", p.got.SourceType)
	}
}

// A text document must NOT pay a base64 encode or trip the Docling backend.
func TestPersistChunks_TextDocumentSendsNoDataB64(t *testing.T) {
	p := &captureParser{}
	im := newCaptureIngestManager(t, p)

	doc := domain.ExternalDocument{
		SourceURI:  "operator_ingest://notes.md",
		SourceType: "operator_ingest",
		Title:      "notes.md",
		Body:       "# Notes\n\nthe sprint slipped",
	}

	if _, err := im.persistChunks(context.Background(), doc, nil, "entity-1", ""); err != nil {
		t.Fatalf("persistChunks: %v", err)
	}

	if p.got.DataB64 != "" {
		t.Errorf("DataB64 must stay empty on the text lane, got %q", p.got.DataB64)
	}
	if p.got.Text != doc.Body {
		t.Errorf("Text: want the body, got %q", p.got.Text)
	}
}
