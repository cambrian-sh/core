package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// T-20261102-ingestion-structure-junk-filter-silently-drops.
//
// The leaves below are what the docling_agent returns for a chat session under
// junk_mode "mark" (agents/system/docling_agent/test_structure.py asserts the same
// classification on the same kinds of text): every block a "paragraph", the
// table, terminal output, number list and receipts list tagged junk_reason.
// Before this change the sidecar DELETED those four leaves.

const (
	chatProse    = "user: I compared the plans, here is the table."
	chatTable    = "| Plan | Price | GB |\n|------|-------|----|\n| A    | $10   | 5  |\n| B    | $20   | 50 |"
	chatTerminal = "$ ls -la /tmp\n-rw-r--r-- 1 me 4096 11-02 10:00 a.txt\ndrwxr-xr-x 2 me 4096 11-02 10:01 d"
	chatNumbers  = "1. 42\n2. 17\n3. 3.14"
	chatReceipts = "- Walmart: $120.00\n- TJ's: $85.40\n- 7-11: $12.99\n- CVS: $23.10"
)

func junkLeaf(id, text, reason string, order int) StructNode {
	n := StructNode{ID: id, Kind: "paragraph", Level: 0, Text: text, ParentID: "doc", Order: order, Path: "n" + string(rune('0'+order))}
	if reason != "" {
		n.Meta = map[string]any{structMetaJunkReason: reason}
	}
	return n
}

func structuredChat(backend string) *StructuredDocument {
	return &StructuredDocument{
		DocID: "chat", Title: "chat", SourceType: "text", Backend: backend, OK: true,
		Nodes: []StructNode{
			{ID: "doc", Kind: "document"},
			junkLeaf("chk:n0", chatProse, "", 0),
			junkLeaf("chk:n1", chatTable, "low_letter_ratio", 1),
			junkLeaf("chk:n2", chatTerminal, "low_letter_ratio", 2),
			junkLeaf("chk:n3", chatNumbers, "few_letters", 3),
			junkLeaf("chk:n4", chatReceipts, "low_letter_ratio", 4),
		},
	}
}

// fixedParser returns a fresh copy of a canned parse and records the request.
type fixedParser struct {
	mu   sync.Mutex
	make func() *StructuredDocument
	got  StructureParseRequest
}

func (p *fixedParser) Parse(_ context.Context, req StructureParseRequest) (*StructuredDocument, error) {
	p.mu.Lock()
	p.got = req
	p.mu.Unlock()
	return p.make(), nil
}

func newJunkTestManager(t *testing.T, p StructureParser, backends []string) (*IngestionManager, *captureAllStore) {
	t.Helper()
	store := &captureAllStore{}
	agent := &Agent{
		Manager:    &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}},
		pendingCap: 256,
	}
	reg, err := NewRegistry(defaultChunkers(), config.ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	im := NewIngestionManagerWithRegistry(nil, &mockEmbedder{vec: []float32{0.1, 0.2}}, agent, reg,
		IngestionConfig{JunkFilterBackends: backends})
	im.SetStructureGraph(p, noopStructStore{})
	return im, store
}

func storedText(store *captureAllStore) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	var b strings.Builder
	for _, d := range store.savedDocs {
		b.WriteString(d.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// A chat session ingested with the SHIPPED default (filter docling only) keeps
// every part: the table, the terminal output, the number list and the receipts.
func TestStructureJunk_ChatSessionKeepsEveryLeaf(t *testing.T) {
	t.Parallel()
	p := &fixedParser{make: func() *StructuredDocument { return structuredChat("text") }}
	defaults := config.DefaultConfig().Execution.Ingestion.StructureJunkFilterBackends
	im, store := newJunkTestManager(t, p, defaults)

	doc := domain.ExternalDocument{
		SourceURI: "longmemeval://session-1", SourceType: "chat", Title: "chat",
		Body: strings.Join([]string{chatProse, chatTable, chatTerminal, chatNumbers, chatReceipts}, "\n\n"),
	}
	out, err := im.persistChunks(context.Background(), doc, nil, "e1", "")
	if err != nil {
		t.Fatalf("persistChunks: %v", err)
	}
	if p.got.JunkMode != structureJunkModeMark {
		t.Errorf("sidecar must be asked to mark, not drop: JunkMode=%q", p.got.JunkMode)
	}
	if out.DroppedLeaves != 0 || im.StructureLeavesDropped() != 0 {
		t.Errorf("chat ingest dropped leaves: outcome=%d total=%d", out.DroppedLeaves, im.StructureLeavesDropped())
	}
	got := storedText(store)
	for name, want := range map[string]string{
		"prose": chatProse, "markdown table": chatTable, "terminal output": chatTerminal,
		"number list": chatNumbers, "receipts list": chatReceipts,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not stored; stored text:\n%s", name, got)
		}
	}
}

// Where the filter legitimately applies (Docling output, configured), classified
// junk is dropped, real content is kept, and the drop is counted.
func TestStructureJunk_DoclingJunkDroppedWhenConfigured(t *testing.T) {
	t.Parallel()
	mk := func() *StructuredDocument {
		return &StructuredDocument{
			DocID: "pdf", Backend: "docling", OK: true,
			Nodes: []StructNode{
				{ID: "doc", Kind: "document"},
				junkLeaf("chk:n0", "Photosynthesis converts light to chemical energy.", "", 0),
				junkLeaf("chk:n1", "<!-- image not available -->", "layout_marker", 1),
				junkLeaf("chk:n2", "! ! !", "few_letters", 2),
			},
		}
	}
	im, store := newJunkTestManager(t, &fixedParser{make: mk}, []string{"docling"})
	doc := domain.ExternalDocument{SourceURI: "operator_ingest://bio.pdf", SourceType: "pdf", Title: "bio", Data: []byte("%PDF")}
	out, err := im.persistChunks(context.Background(), doc, nil, "e1", "")
	if err != nil {
		t.Fatalf("persistChunks: %v", err)
	}
	if out.DroppedLeaves != 2 || im.StructureLeavesDropped() != 2 {
		t.Fatalf("want 2 dropped, got outcome=%d total=%d", out.DroppedLeaves, im.StructureLeavesDropped())
	}
	got := storedText(store)
	if !strings.Contains(got, "Photosynthesis") {
		t.Errorf("real content lost: %q", got)
	}
	if strings.Contains(got, "image not available") || strings.Contains(got, "! ! !") {
		t.Errorf("Docling junk was stored: %q", got)
	}

	// The same Docling parse with the filter disabled ([]) keeps everything.
	im2, store2 := newJunkTestManager(t, &fixedParser{make: mk}, nil)
	if out, err := im2.persistChunks(context.Background(), doc, nil, "e1", ""); err != nil || out.DroppedLeaves != 0 {
		t.Fatalf("filter off: dropped=%d err=%v", out.DroppedLeaves, err)
	}
	if !strings.Contains(storedText(store2), "image not available") {
		t.Error("with no backends configured, nothing may be dropped")
	}
}

// lockedBuffer makes a log sink safe against concurrent writers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Every drop is auditable: document id, leaf kind, reason, length, a content
// hash and a truncated sample — WARN per document, DEBUG per leaf. A legacy
// sidecar's in-process drops are still reported and counted.
//
// Not parallel: it swaps the default logger.
func TestStructureJunk_DropsAreLoggedWithHashAndSample(t *testing.T) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	long := strings.Repeat("| 1 | 2 | 3 | ", 40) // > junkSampleRunes, so the sample is truncated
	sd := &StructuredDocument{
		Backend: "text", DroppedLeaves: 3, // 3 dropped by a legacy sidecar
		Nodes: []StructNode{
			{ID: "doc", Kind: "document"},
			junkLeaf("chk:n0", chatReceipts, "low_letter_ratio", 0),
			junkLeaf("chk:n1", long, "low_letter_ratio", 1),
			junkLeaf("chk:n2", "a real sentence about the plan", "", 2),
		},
	}
	im := NewIngestionManagerWithRegistry(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(), nil,
		IngestionConfig{JunkFilterBackends: []string{"text"}})

	n := im.applyStructureJunkFilter(context.Background(), "doc-42", sd)
	if n != 5 || im.StructureLeavesDropped() != 5 {
		t.Fatalf("want 5 dropped (2 kernel + 3 legacy), got %d / total %d", n, im.StructureLeavesDropped())
	}
	if len(sd.OrderedLeaves()) != 1 {
		t.Fatalf("want only the real sentence left, got %+v", sd.OrderedLeaves())
	}
	logs := buf.String()
	for _, want := range []string{
		"structure junk filter dropped content leaves", "doc=doc-42", "dropped_leaves=2",
		"paragraph/low_letter_ratio", "sha256=" + leafHash(chatReceipts), "Walmart: $120.00",
		"len=" + strconv.Itoa(len(chatReceipts)),
		"structure junk leaf dropped", "level=DEBUG",
		"legacy sidecar", "dropped_leaves=3",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, long) {
		t.Error("a long leaf must be logged as a truncated sample, not in full")
	}
	if !strings.Contains(logs, "…") {
		t.Error("truncated sample must be marked with …")
	}
}

// Classified leaves of an UNFILTERED backend are kept and only visible at DEBUG.
func TestStructureJunk_UnfilteredBackendKeepsAndStaysOffInfo(t *testing.T) {
	t.Parallel()
	im := NewIngestionManagerWithRegistry(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(), nil,
		IngestionConfig{JunkFilterBackends: []string{"docling"}})
	sd := structuredChat("markdown")
	if n := im.applyStructureJunkFilter(context.Background(), "chat", sd); n != 0 {
		t.Fatalf("markdown backend must drop nothing, dropped %d", n)
	}
	if got := len(sd.OrderedLeaves()); got != 5 {
		t.Fatalf("want 5 leaves kept, got %d", got)
	}
}

func TestLeafSample_RuneSafeTruncation(t *testing.T) {
	t.Parallel()
	s := leafSample(strings.Repeat("ğ", junkSampleRunes+10))
	if !strings.HasSuffix(s, "…") || len([]rune(s)) != junkSampleRunes+1 {
		t.Fatalf("bad sample: %d runes", len([]rune(s)))
	}
	if leafSample("a\n\n  b") != "a b" {
		t.Fatalf("whitespace not collapsed: %q", leafSample("a\n\n  b"))
	}
}
