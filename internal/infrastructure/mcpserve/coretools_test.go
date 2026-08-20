package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// ── fakes for the four backend ports ─────────────────────────────────────────

type fakeSearcher struct {
	results   []domain.SearchResult
	err       error
	gotQuery  string
	gotCaller string
}

func (f *fakeSearcher) Search(_ context.Context, query, callerID string) ([]domain.SearchResult, error) {
	f.gotQuery, f.gotCaller = query, callerID
	return f.results, f.err
}

type fakeAnswerer struct {
	status    string
	answer    string
	evidence  []domain.SearchResult
	err       error
	gotQuery  string
	gotCaller string
}

func (f *fakeAnswerer) Answer(_ context.Context, query, callerID string) (string, string, []domain.SearchResult, error) {
	f.gotQuery, f.gotCaller = query, callerID
	return f.status, f.answer, f.evidence, f.err
}

type fakeLister struct {
	page      []memory.DocumentSummary
	next      string
	total     int
	gotFilter memory.DocumentFilter
}

func (f *fakeLister) ListDocuments(_ context.Context, filter memory.DocumentFilter) ([]memory.DocumentSummary, string, int, error) {
	f.gotFilter = filter
	return f.page, f.next, f.total, nil
}

type fakeGetter struct {
	doc          domain.Document
	err          error
	gotPrincipal domain.PrincipalRef
	gotID        string
}

func (f *fakeGetter) GetDocument(_ context.Context, principal domain.PrincipalRef, id string) (domain.Document, error) {
	f.gotPrincipal, f.gotID = principal, id
	return f.doc, f.err
}

// callerCtx is a context the D4 middleware would have produced.
func callerCtx() context.Context {
	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("mcp:ci-bot"))
	return domain.WithSurface(ctx, domain.SurfaceRef{Kind: domain.SurfaceMCP, ID: surfaceID})
}

func results(n int) []domain.SearchResult {
	out := make([]domain.SearchResult, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.SearchResult{
			Score: 0.9 - float64(i)/100,
			Document: domain.Document{
				ID:          "doc-" + string(rune('a'+i)),
				Text:        "passage " + string(rune('a'+i)),
				SectionPath: "s/p",
				Metadata:    map[string]any{"tags": []any{"eng"}},
			},
		})
	}
	return out
}

func structured(t *testing.T, res domain.PublishedToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.Structured)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return m
}

// ── search_memory ────────────────────────────────────────────────────────────

func TestSearchMemory_ScopesToThePrincipalAndMapsResults(t *testing.T) {
	search := &fakeSearcher{results: results(3)}
	b := NewCoreBackends()
	b.Bind(search, nil, nil, nil)

	res, err := b.searchMemory(callerCtx(), json.RawMessage(`{"query":"invoices"}`))
	if err != nil {
		t.Fatalf("searchMemory: %v", err)
	}
	// The caller id is the PRINCIPAL off the context — D4's property, and what a
	// scoped deployment narrows on.
	if search.gotCaller != "mcp:ci-bot" || search.gotQuery != "invoices" {
		t.Errorf("backend saw (query=%q caller=%q)", search.gotQuery, search.gotCaller)
	}
	m := structured(t, res)
	if m["count"].(float64) != 3 {
		t.Errorf("count = %v", m["count"])
	}
	first := m["results"].([]any)[0].(map[string]any)
	for _, key := range []string{"doc_id", "text", "section_path", "score", "tags"} {
		if _, ok := first[key]; !ok {
			t.Errorf("result row lacks %q: %v", key, first)
		}
	}
}

func TestSearchMemory_CapsTopK(t *testing.T) {
	search := &fakeSearcher{results: results(30)}
	b := NewCoreBackends()
	b.Bind(search, nil, nil, nil)

	res, err := b.searchMemory(callerCtx(), json.RawMessage(`{"query":"q","top_k":1000}`))
	if err != nil {
		t.Fatalf("searchMemory: %v", err)
	}
	if got := structured(t, res)["count"].(float64); got != float64(searchMaxTopK) {
		t.Errorf("count = %v, want the cap %d", got, searchMaxTopK)
	}
}

func TestSearchMemory_RequiresAQuery(t *testing.T) {
	b := NewCoreBackends()
	b.Bind(&fakeSearcher{}, nil, nil, nil)
	if _, err := b.searchMemory(callerCtx(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("an empty query was accepted")
	}
}

func TestCoreTools_UnboundBackendsAnswerNotReadyPerTool(t *testing.T) {
	b := NewCoreBackends() // never bound
	for name, call := range map[string]func() (domain.PublishedToolResult, error){
		"search_memory": func() (domain.PublishedToolResult, error) {
			return b.searchMemory(callerCtx(), []byte(`{"query":"q"}`))
		},
		"ask_memory": func() (domain.PublishedToolResult, error) { return b.askMemory(callerCtx(), []byte(`{"query":"q"}`)) },
		"get_document": func() (domain.PublishedToolResult, error) {
			return b.getDocument(callerCtx(), []byte(`{"doc_id":"d"}`))
		},
		"list_documents": func() (domain.PublishedToolResult, error) { return b.listDocuments(callerCtx(), []byte(`{}`)) },
	} {
		if _, err := call(); !errors.Is(err, errNotReady) {
			t.Errorf("%s: err = %v, want errNotReady", name, err)
		}
	}
}

// ── ask_memory ───────────────────────────────────────────────────────────────

func TestAskMemory_SynthesizedAnswerCarriesNumberedCitations(t *testing.T) {
	b := NewCoreBackends()
	b.Bind(&fakeSearcher{}, &fakeAnswerer{
		status: "ok", answer: "Paid on time [1], twice [2].", evidence: results(5),
	}, nil, nil)

	res, err := b.askMemory(callerCtx(), json.RawMessage(`{"query":"were invoices paid?","top_k":2}`))
	if err != nil {
		t.Fatalf("askMemory: %v", err)
	}
	m := structured(t, res)
	if m["extractive"] != false || m["status"] != "ok" {
		t.Errorf("extractive/status = %v/%v", m["extractive"], m["status"])
	}
	cites := m["citations"].([]any)
	if len(cites) != 2 {
		t.Fatalf("citations = %d, want the top_k cap of 2", len(cites))
	}
	// [n] resolves to citations[n-1]: markers are 1-based and dense.
	for i, c := range cites {
		if n := c.(map[string]any)["n"].(float64); n != float64(i+1) {
			t.Errorf("citations[%d].n = %v", i, n)
		}
	}
	if res.Text != "Paid on time [1], twice [2]." {
		t.Errorf("text = %q", res.Text)
	}
}

// ADR-0126 E6: ask_memory is the one tool that hands back PROSE, so an unscoped
// answer lane leaks harder than an unscoped search — the answer text survives even
// when the citations are ones the caller could not have fetched. The caller id must
// come off the context, exactly as it does for search_memory.
func TestAskMemory_ScopesTheAnswerLaneByTheCallingPrincipal(t *testing.T) {
	answerer := &fakeAnswerer{status: "ok", answer: "grounded [1]", evidence: results(1)}
	b := NewCoreBackends()
	b.Bind(&fakeSearcher{}, answerer, nil, nil)

	if _, err := b.askMemory(callerCtx(), json.RawMessage(`{"query":"who?"}`)); err != nil {
		t.Fatalf("askMemory: %v", err)
	}
	if answerer.gotCaller != "mcp:ci-bot" {
		t.Errorf("answer lane saw caller %q, want the context principal mcp:ci-bot", answerer.gotCaller)
	}
	if answerer.gotQuery != "who?" {
		t.Errorf("answer lane saw query %q", answerer.gotQuery)
	}
}

func TestAskMemory_NoModelDegradesToAnExtractiveAnswer(t *testing.T) {
	// answer lane nil = no synthesis model. NEVER an error (a no-LLM kernel is a
	// supported deployment) and never MCP sampling (deprecated, spec 2026-07-28).
	b := NewCoreBackends()
	b.Bind(&fakeSearcher{results: results(8)}, nil, nil, nil)

	res, err := b.askMemory(callerCtx(), json.RawMessage(`{"query":"anything"}`))
	if err != nil {
		t.Fatalf("askMemory (extractive): %v", err)
	}
	m := structured(t, res)
	if m["extractive"] != true || m["status"] != "extractive" {
		t.Errorf("extractive/status = %v/%v", m["extractive"], m["status"])
	}
	if got := len(m["citations"].([]any)); got != extractivePassages {
		t.Errorf("citations = %d, want %d", got, extractivePassages)
	}
	// The degradation announces itself and keeps the [n] contract.
	if !strings.Contains(res.Text, "No synthesis model") || !strings.Contains(res.Text, "[1]") {
		t.Errorf("extractive text = %q", res.Text)
	}
}

func TestAskMemory_ImposesTheAnswerBudget(t *testing.T) {
	b := NewCoreBackends()
	b.Bind(&fakeSearcher{}, deadlineProbe{}, nil, nil)
	if _, err := b.askMemory(callerCtx(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("askMemory: %v", err)
	}
}

// deadlineProbe fails the answer if no deadline (≤ the 90s budget) was imposed.
type deadlineProbe struct{}

func (deadlineProbe) Answer(ctx context.Context, _, _ string) (string, string, []domain.SearchResult, error) {
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > answerBudget {
		return "", "", nil, errors.New("no answer budget on the context")
	}
	return "ok", "fine", nil, nil
}

// ── get_document ─────────────────────────────────────────────────────────────

func TestGetDocument_PagesByRunesAndReportsTruncation(t *testing.T) {
	// Multi-byte text, so byte-based slicing would split a rune and corrupt the
	// page boundary — offsets are characters by contract.
	getter := &fakeGetter{doc: domain.Document{ID: "doc-1", Text: "éééééééééé"}} // 10 runes, 20 bytes
	b := NewCoreBackends()
	b.Bind(nil, nil, nil, getter)

	res, err := b.getDocument(callerCtx(), json.RawMessage(`{"doc_id":"doc-1","max_chars":4,"offset":3}`))
	if err != nil {
		t.Fatalf("getDocument: %v", err)
	}
	if getter.gotPrincipal.ID != "mcp:ci-bot" || getter.gotID != "doc-1" {
		t.Errorf("backend saw principal=%v id=%q", getter.gotPrincipal, getter.gotID)
	}
	m := structured(t, res)
	if m["text"] != "éééé" || m["returned_chars"].(float64) != 4 ||
		m["total_chars"].(float64) != 10 || m["truncated"] != true || m["offset"].(float64) != 3 {
		t.Errorf("paging = %v", m)
	}
}

func TestGetDocument_NotFoundPassesThroughUnchanged(t *testing.T) {
	// Absent and unreadable are the SAME answer by the port's contract; this
	// handler must not editorialize the distinction back in.
	getter := &fakeGetter{err: domain.ErrDocumentNotFound}
	b := NewCoreBackends()
	b.Bind(nil, nil, nil, getter)
	if _, err := b.getDocument(callerCtx(), json.RawMessage(`{"doc_id":"nope"}`)); !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("err = %v, want ErrDocumentNotFound", err)
	}
}

// ── list_documents ───────────────────────────────────────────────────────────

func TestListDocuments_MapsTheFilterAndThePage(t *testing.T) {
	lister := &fakeLister{
		page: []memory.DocumentSummary{{
			ID: "doc-1", Title: "Q3 invoices", SourceType: "upload",
			Tags: []string{"finance"}, ChunkCount: 4, CreatedAt: time.UnixMilli(1_755_000_000_000),
		}},
		next: "doc-1", total: 61,
	}
	b := NewCoreBackends()
	b.Bind(nil, nil, lister, nil)

	res, err := b.listDocuments(callerCtx(),
		json.RawMessage(`{"limit":500,"cursor":"c0","unlabelled_only":true,"id_prefix":"doc","tags":["finance","q3"]}`))
	if err != nil {
		t.Fatalf("listDocuments: %v", err)
	}
	f := lister.gotFilter
	if f.Limit != listMaxLimit || f.Cursor != "c0" || !f.UnlabelledOnly || f.IDPrefix != "doc" || len(f.Tags) != 2 {
		t.Errorf("filter = %+v (limit must be capped at %d)", f, listMaxLimit)
	}
	m := structured(t, res)
	if m["next_cursor"] != "doc-1" || m["total_matching"].(float64) != 61 {
		t.Errorf("page meta = %v", m)
	}
	row := m["documents"].([]any)[0].(map[string]any)
	if row["id"] != "doc-1" || row["chunk_count"].(float64) != 4 || row["created_at_unix_ms"].(float64) != 1_755_000_000_000 {
		t.Errorf("row = %v", row)
	}
}
