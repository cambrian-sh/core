package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/evidence"
)

// scriptedParser answers the structure parse the way the docling sidecar does for
// the inputs of the ENG-2110 readiness block.
type scriptedParser struct {
	doc *StructuredDocument
	err error
}

func (p scriptedParser) Parse(_ context.Context, req StructureParseRequest) (*StructuredDocument, error) {
	if p.err != nil || p.doc == nil {
		return p.doc, p.err
	}
	d := *p.doc
	d.DocID = req.DocID
	return &d, nil
}

// countingEvidence counts evidence writes; a refused ingest must make none.
type countingEvidence struct {
	mu sync.Mutex
	n  int
}

func (c *countingEvidence) Ingest(context.Context, evidence.Raw) (domain.EvidenceID, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return "ev-1", true, nil
}

func leafDoc(ok bool, texts ...string) *StructuredDocument {
	d := &StructuredDocument{Backend: "docling", OK: ok}
	for i, t := range texts {
		d.Nodes = append(d.Nodes, StructNode{ID: "n" + string(rune('a'+i)), Kind: "paragraph", Text: t, Order: i})
	}
	return d
}

// ENG-2110: every bad input of the readiness block is refused with a reason that
// names the document, and nothing — evidence, chunk, journal — is written.
func TestProcessSync_RefusesUnusableContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		doc    domain.ExternalDocument
		parser StructureParser // nil = no structure parser configured
		why    string
	}{
		{"whitespace text", domain.ExternalDocument{SourceURI: "operator_ingest://s1", SourceType: "operator_ingest", Title: "notes", Body: "   \n\t  "},
			scriptedParser{doc: leafDoc(true)}, "whitespace only"},
		{"NUL-only text", domain.ExternalDocument{SourceURI: "operator_ingest://s2", SourceType: "operator_ingest", Title: "nul", Body: "\x00\x00 \x00"},
			nil, "whitespace only"},
		{"random bytes .bin, parser failed", domain.ExternalDocument{SourceURI: "operator_ingest://blob.bin", SourceType: "bin", UploadName: "blob.bin", Data: []byte{0x8f, 0x01, 0xfe, 0x00}},
			scriptedParser{err: errors.New("docling: unsupported format")}, "document parser failed"},
		{"random bytes named .pdf, degraded parse", domain.ExternalDocument{SourceURI: "operator_ingest://fake.pdf", SourceType: "pdf", UploadName: "fake.pdf", Data: []byte("not a pdf")},
			scriptedParser{doc: leafDoc(false)}, "degraded parse"},
		{"truncated pdf, degraded parse with blank leaves", domain.ExternalDocument{SourceURI: "operator_ingest://cut.pdf", SourceType: "pdf", UploadName: "cut.pdf", Data: []byte("%PDF-1.7\n1 0 obj")},
			scriptedParser{doc: leafDoc(false, "  ", "\n")}, "degraded parse"},
		{"png with no OCR text, note in body", domain.ExternalDocument{SourceURI: "operator_ingest://dot.png", SourceType: "png", UploadName: "dot.png", Data: []byte("\x89PNG\r\n"), Body: "operator note"},
			scriptedParser{doc: leafDoc(true)}, "found no text"},
		{"extension-less bytes, no parser", domain.ExternalDocument{SourceURI: "operator_ingest://noext", SourceType: "operator_ingest", UploadName: "noext", Data: []byte{1, 2, 3, 4, 5}},
			nil, "none is configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &captureAllStore{}
			im := newCaptureIngestManager(t, nil)
			im.structureParser, im.structureStore = nil, nil
			if tc.parser != nil {
				im.SetStructureGraph(tc.parser, noopStructStore{})
			}
			im.agent.Manager.Store = store
			ev := &countingEvidence{}
			im.SetEvidenceCapture(ev)
			_, err := im.ProcessSync(context.Background(), tc.doc)
			if !errors.Is(err, domain.ErrNoUsableText) {
				t.Fatalf("want ErrNoUsableText, got %v", err)
			}
			if msg := err.Error(); !strings.HasPrefix(msg, "no usable text could be extracted from "+tc.doc.IngestDisplayName()+": ") ||
				!strings.Contains(msg, tc.why) {
				t.Errorf("reason %q: want the document name and %q", msg, tc.why)
			}
			if ev.n != 0 {
				t.Errorf("evidence written for a refused ingest: %d", ev.n)
			}
			if got := len(store.snapshot()); got != 0 {
				t.Errorf("%d rows saved for a refused ingest", got)
			}
		})
	}
}

// A good document with the same wiring still ingests, and a NUL inside real text is
// normalised instead of reaching Postgres.
func TestProcessSync_GoodAndNULTextIngest(t *testing.T) {
	t.Parallel()
	store := &captureAllStore{}
	im := newCaptureIngestManager(t, nil)
	im.structureParser, im.structureStore = nil, nil
	im.agent.Manager.Store = store
	ev := &countingEvidence{}
	im.SetEvidenceCapture(ev)
	if _, err := im.ProcessSync(context.Background(), domain.ExternalDocument{
		SourceURI: "operator_ingest://good", SourceType: "operator_ingest", Title: "good",
		Body: "the quarterly report\x00 is due on Friday",
	}); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if ev.n != 1 {
		t.Errorf("evidence writes: %d, want 1", ev.n)
	}
	saved := store.snapshot()
	if len(saved) == 0 {
		t.Fatal("no chunk saved")
	}
	for _, d := range saved {
		if strings.ContainsRune(d.Text, 0) {
			t.Errorf("chunk %s still carries a NUL: %q", d.ID, d.Text)
		}
	}
}

// The batch lane (/v1/ingest) drops an unusable document with nothing written.
func TestProcessBatchDoc_DropsUnusableContent(t *testing.T) {
	t.Parallel()
	store := &captureAllStore{}
	im := newCaptureIngestManager(t, scriptedParser{doc: leafDoc(false)})
	im.agent.Manager.Store = store
	im.processBatchDoc(context.Background(), domain.ExternalDocument{SourceURI: "https://x/blob.pdf", SourceType: "pdf", Data: []byte("junk")})
	im.processBatchDoc(context.Background(), domain.ExternalDocument{SourceURI: "https://x/ws", SourceType: "web", Body: " \t\n"})
	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("%d rows saved for refused queued documents", got)
	}
	im.processBatchDoc(context.Background(), domain.ExternalDocument{SourceURI: "https://x/ok", SourceType: "web", Title: "ok", Body: "the build passed"})
	if len(store.snapshot()) == 0 {
		t.Fatal("a good queued document stored nothing")
	}
}

// ENG-2110: /v1/ingest decodes strictly, refuses a push with nothing usable, and
// answers 413 when the listener's body cap is hit.
func TestWebhookReceiver_RefusesUnusablePushes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		limit      int64
		code       int
		want       string
	}{
		{"unknown field", `{"content":"hello"}`, 0, http.StatusBadRequest, `unknown field "content"`},
		{"no body at all", `{"Title":"t"}`, 0, http.StatusBadRequest, "no usable text could be extracted from t: the body is empty"},
		{"whitespace body", `{"Body":"  \n\t "}`, 0, http.StatusBadRequest, "whitespace only"},
		{"NUL-only body", `{"Body":"\u0000"}`, 0, http.StatusBadRequest, "whitespace only"},
		{"oversized", `{"Body":"` + strings.Repeat("a", 2048) + `"}`, 1024, http.StatusRequestEntityTooLarge, "limit is 1024 bytes"},
		{"good text", `{"Body":"the build passed"}`, 0, http.StatusAccepted, "queued"},
		{"bytes only", `{"Data":"aGVsbG8=","SourceURI":"https://x/a.pdf"}`, 0, http.StatusAccepted, "queued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enqueued := 0
			h := http.Handler(NewWebhookReceiver("tok", func(context.Context, domain.ExternalDocument) bool { enqueued++; return true }))
			if tc.limit > 0 {
				inner := h
				h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.Body = http.MaxBytesReader(w, r.Body, tc.limit)
					inner.ServeHTTP(w, r)
				})
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(tc.body))
			req.Header.Set("X-Ingest-Token", "tok")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status %d body %q; want %d containing %q", rec.Code, rec.Body.String(), tc.code, tc.want)
			}
			if wantEnq := tc.code == http.StatusAccepted; (enqueued == 1) != wantEnq {
				t.Errorf("enqueued=%d for status %d", enqueued, rec.Code)
			}
		})
	}
}
