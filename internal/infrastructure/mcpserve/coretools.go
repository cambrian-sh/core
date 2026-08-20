package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// The four read-only core tools (ADR-0126 D5, phase 1).
//
// They are defined HERE, beside the renderer, rather than registered through
// Registry.PublishTool: that seam exists for plugins, whose entitlement is
// evaluated before registration — the core tools are unconditional product
// surface, and routing them through the plugin chokepoint would only add a
// fake plugin for the chokepoint to wave through. The golden contract test
// lives beside them for the same reason: these declarations are the public
// contract, and the test that freezes them should not need the composition
// root to compile.
//
// `remember` is DELIBERATELY absent (owner ruling, 2026-08-14): phase 1 is
// read-only. Its shipping contract — the provenance triple and idempotency —
// is recorded in ADR-0126 D5 for the phase that lands it.

// CoreInstructions is the D12 server-instructions text, returned to every
// client at initialize. It is the single highest-leverage string in this
// package: tools that are listed but never called are the observed failure
// mode of memory servers, and this is the text that stands between the two.
const CoreInstructions = `Cambrian is this deployment's shared, cited memory. Prefer querying it over guessing about prior context.

- search_memory: cheap, fast, no LLM. The default for lookups and anything exploratory.
- ask_memory: a grounded prose answer with [n] citation markers; runs retrieval plus an LLM synthesis under a ~90 second budget. Use it when you need synthesis across sources, not for simple lookups. Every [n] resolves to an entry in citations[]; follow a citation's doc_id with get_document to read the source.
- get_document: fetch a document body by doc_id (paged via offset/max_chars).
- list_documents: browse the corpus by row — labels, pagination — for the questions search cannot ask, like "which documents are unlabelled?".

Results are scoped to your identity: an empty result can mean "nothing stored" or "not visible to you", and the two are deliberately indistinguishable. When you rely on retrieved content, cite its doc_id.`

// CoreBackends is the binding seam between the endpoint and the memory
// subsystem. The composition root binds it before the endpoint starts, so in
// a full kernel the tools serve real answers from the first request; the
// indirection exists because this package cannot import the composition root,
// and because a deployment missing a piece (no memory pipeline, no document
// store) must degrade PER TOOL with a retryable "not available" answer rather
// than refuse to serve — the same stable-value-then-populate shape the ADR
// names for plugins.
type CoreBackends struct {
	mu sync.RWMutex
	// search is the caller-scoped recall seam (*memory.QueryService.Search).
	search MemorySearcher
	// answer is the agentic answer lane. nil when the deployment has no
	// synthesis model or agentic retrieval is off — ask_memory then degrades to
	// an extractive answer rather than failing (D12: no sampling, the spec
	// deprecated it).
	answer MemoryAnswerer
	// documents enumerates by row; docs resolves a body by id, principal-scoped.
	documents memory.DocumentLister
	docs      domain.DocumentGetter
}

// MemorySearcher is the scoped recall port. Satisfied by *memory.QueryService.
type MemorySearcher interface {
	Search(ctx context.Context, query, callerID string) ([]domain.SearchResult, error)
}

// MemoryAnswerer is the grounded answer port (ADR-0081). Satisfied by
// *memory.QueryService when the agentic path is wired.
//
// It takes a callerID for the same reason MemorySearcher does, and E6 added the
// backing variant so it could: this port originally bound to the SYSTEM-scoped
// AnswerSystem, which under a real PDP would have let every external caller
// synthesize prose over the whole corpus — a stronger disclosure than the search
// tool beside it, because the answer text survives even when the citations are
// ones the caller could not have fetched. The four core tools now take the same
// per-caller narrowing without exception.
type MemoryAnswerer interface {
	Answer(ctx context.Context, query, callerID string) (status, answer string, evidence []domain.SearchResult, err error)
}

// NewCoreBackends returns an empty, bindable backend set.
func NewCoreBackends() *CoreBackends { return &CoreBackends{} }

// Bind attaches the live services. Any argument may be nil; the corresponding
// tool keeps answering "not available" — per call and per tool, so a kernel
// with search but no document store still serves the tools it can.
func (b *CoreBackends) Bind(search MemorySearcher, answer MemoryAnswerer, documents memory.DocumentLister, docs domain.DocumentGetter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.search, b.answer, b.documents, b.docs = search, answer, documents, docs
}

func (b *CoreBackends) snapshot() (MemorySearcher, MemoryAnswerer, memory.DocumentLister, domain.DocumentGetter) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.search, b.answer, b.documents, b.docs
}

// errNotReady is the per-call answer while a backend is absent. Worded for the
// calling MODEL: it must be able to tell "retry shortly" from "give up".
var errNotReady = errors.New("memory is not available on this kernel yet — " +
	"if the kernel is still starting this resolves in under a minute (retry); " +
	"if it persists, this deployment has no memory pipeline configured")

// Argument caps. Defaults are chosen for an LLM caller's context budget;
// caps keep one call from becoming a corpus export.
const (
	searchDefaultTopK = 8
	searchMaxTopK     = 25
	askDefaultTopK    = 8
	askMaxTopK        = 20
	getDefaultChars   = 20_000
	getMaxChars       = 200_000
	listDefaultLimit  = 50
	listMaxLimit      = 200
	// extractivePassages is how many passages the no-LLM fallback returns.
	extractivePassages = 5
	// extractivePassageChars truncates each fallback passage.
	extractivePassageChars = 700
	// answerBudget mirrors the operator answer lane's budget: retrieval plus
	// one grounded synthesis, and the LLM stream reads its timeout from the
	// remaining deadline — unbounded would leave it ill-defined.
	answerBudget = 90 * time.Second
)

// CoreTools renders the four read-only tools over b. The returned entries are
// owner "core" and join the plugin-published surface in the composition root.
func CoreTools(b *CoreBackends) domain.PublishedToolSurface {
	return domain.PublishedToolSurface{
		{Owner: "core", Tool: searchMemoryTool, Handler: handlerFunc(b.searchMemory)},
		{Owner: "core", Tool: askMemoryTool, Handler: handlerFunc(b.askMemory)},
		{Owner: "core", Tool: getDocumentTool, Handler: handlerFunc(b.getDocument)},
		{Owner: "core", Tool: listDocumentsTool, Handler: handlerFunc(b.listDocuments)},
	}
}

// handlerFunc adapts a method to domain.PublishedToolHandler.
type handlerFunc func(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error)

func (f handlerFunc) Invoke(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	return f(ctx, args)
}

// ── declarations ─────────────────────────────────────────────────────────────
//
// These schemas are a PUBLIC CONTRACT the moment the golden test freezes them:
// external clients Cambrian does not ship bind to every property name. Extend,
// never mutate. Size/offset parameters exist from day one because coding-agent
// clients truncate tool results around 25k tokens, and retrofitting pagination
// later IS a mutation.

var searchMemoryTool = domain.PublishedTool{
	Name:  "search_memory",
	Title: "Search memory",
	Description: "Semantic search over this deployment's memory. Cheap and fast (no LLM) — " +
		"the default for lookups. Returns scored passages with doc_id, section_path and tags; " +
		"follow a doc_id with get_document for the full source.",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["query"],
  "properties": {
    "query": {"type": "string", "description": "What to search for, in natural language."},
    "top_k": {"type": "integer", "minimum": 1, "maximum": 25, "default": 8, "description": "Maximum results to return."}
  }
}`),
	Effects:  []domain.ToolEffect{domain.EffectRead},
	ReadOnly: true,
}

var askMemoryTool = domain.PublishedTool{
	Name:  "ask_memory",
	Title: "Ask memory",
	Description: "A grounded prose answer with inline [n] citation markers, synthesized over " +
		"multi-hop retrieval. Invokes an LLM under a ~90 second budget — use search_memory for " +
		"simple lookups. Each [n] resolves to citations[n-1]; when no synthesis model is " +
		"configured the answer degrades to the top passages verbatim (extractive: true).",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["query"],
  "properties": {
    "query": {"type": "string", "description": "The question to answer from memory."},
    "top_k": {"type": "integer", "minimum": 1, "maximum": 20, "default": 8, "description": "Maximum citations to return."}
  }
}`),
	Effects:  []domain.ToolEffect{domain.EffectRead},
	ReadOnly: true,
}

var getDocumentTool = domain.PublishedTool{
	Name:  "get_document",
	Title: "Get document",
	Description: "Fetch a document's body by doc_id (as cited by search_memory, ask_memory or " +
		"list_documents). Paged: offset/max_chars are in characters, and truncated=true means " +
		"there is more — repeat with offset advanced by returned_chars. Not found and not " +
		"readable by you are deliberately the same answer.",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["doc_id"],
  "properties": {
    "doc_id": {"type": "string", "description": "The document id to fetch."},
    "max_chars": {"type": "integer", "minimum": 1, "maximum": 200000, "default": 20000, "description": "Maximum characters of body to return."},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Character offset to start from, for paging through long documents."}
  }
}`),
	Effects:  []domain.ToolEffect{domain.EffectRead},
	ReadOnly: true,
}

var listDocumentsTool = domain.PublishedTool{
	Name:  "list_documents",
	Title: "List documents",
	Description: "Enumerate ingested documents by row — no query text, no ranking. For the " +
		"questions search cannot ask: what is here, what carries a given label, what is " +
		"unlabelled. Keyset-paged via cursor/next_cursor; bodies are never included (use " +
		"get_document).",
	InputSchema: []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "default": 50, "description": "Page size."},
    "cursor": {"type": "string", "description": "Opaque next_cursor from the previous page; empty for the first page."},
    "unlabelled_only": {"type": "boolean", "default": false, "description": "Only documents carrying no labels."},
    "id_prefix": {"type": "string", "description": "Cheap doc_id prefix filter."},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Only documents carrying ALL of these labels."}
  }
}`),
	Effects:  []domain.ToolEffect{domain.EffectRead},
	ReadOnly: true,
}

// ── handlers ─────────────────────────────────────────────────────────────────

// searchHit is one search_memory result row.
type searchHit struct {
	DocID       string   `json:"doc_id"`
	Text        string   `json:"text"`
	SectionPath string   `json:"section_path,omitempty"`
	Score       float64  `json:"score"`
	Tags        []string `json:"tags,omitempty"`
}

func (b *CoreBackends) searchMemory(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	search, _, _, _ := b.snapshot()
	if search == nil {
		return domain.PublishedToolResult{}, errNotReady
	}
	var in struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return domain.PublishedToolResult{}, fmt.Errorf("query is required")
	}
	topK := clamp(in.TopK, searchDefaultTopK, searchMaxTopK)

	// The caller id is the PRINCIPAL, off the context the middleware built —
	// never an argument (D4). It is what a scoped deployment narrows on.
	results, err := search.Search(ctx, in.Query, domain.PrincipalFromContext(ctx).ID)
	if err != nil {
		return domain.PublishedToolResult{}, fmt.Errorf("search: %w", err)
	}
	hits := make([]searchHit, 0, topK)
	for _, r := range results {
		hits = append(hits, searchHit{
			DocID:       r.Document.ID,
			Text:        r.Document.Text,
			SectionPath: r.Document.SectionPath,
			Score:       r.Score,
			Tags:        docTags(r.Document),
		})
		if len(hits) >= topK {
			break
		}
	}
	return domain.PublishedToolResult{
		Structured: map[string]any{"results": hits, "count": len(hits)},
		Text:       fmt.Sprintf("%d result(s) for %q", len(hits), in.Query),
	}, nil
}

// citation is one resolved [n] marker.
type citation struct {
	N           int      `json:"n"`
	DocID       string   `json:"doc_id"`
	Snippet     string   `json:"snippet"`
	SectionPath string   `json:"section_path,omitempty"`
	Score       float64  `json:"score"`
	Tags        []string `json:"tags,omitempty"`
}

func (b *CoreBackends) askMemory(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	search, answer, _, _ := b.snapshot()
	if search == nil && answer == nil {
		return domain.PublishedToolResult{}, errNotReady
	}
	var in struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return domain.PublishedToolResult{}, fmt.Errorf("query is required")
	}
	topK := clamp(in.TopK, askDefaultTopK, askMaxTopK)

	// The synthesis stream reads its timeout from the remaining deadline; give
	// it the same well-defined budget the operator lane uses, shortening only
	// if the caller already imposed something tighter.
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > answerBudget {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, answerBudget)
		defer cancel()
	}

	if answer == nil {
		// No synthesis model: extractive degradation, clearly labelled — never
		// an error (a no-LLM kernel is a supported deployment, not a broken
		// one) and never MCP sampling (deprecated, spec 2026-07-28).
		return b.extractiveAnswer(ctx, search, in.Query)
	}

	// The caller id is the PRINCIPAL off the context, exactly as in searchMemory —
	// never an argument (D4). Passing it is what makes a premium PDP bite on this
	// tool rather than answering every caller at system scope.
	status, text, evidence, err := answer.Answer(ctx, in.Query, domain.PrincipalFromContext(ctx).ID)
	if err != nil {
		return domain.PublishedToolResult{}, fmt.Errorf("answer: %w", err)
	}
	cites := make([]citation, 0, min(topK, len(evidence)))
	for _, r := range evidence {
		cites = append(cites, citation{
			N:           len(cites) + 1,
			DocID:       r.Document.ID,
			Snippet:     truncate(r.Document.Text, extractivePassageChars),
			SectionPath: r.Document.SectionPath,
			Score:       r.Score,
			Tags:        docTags(r.Document),
		})
		if len(cites) >= topK {
			break
		}
	}
	return domain.PublishedToolResult{
		Structured: map[string]any{
			"answer": text, "status": status, "extractive": false, "citations": cites,
		},
		Text: text,
	}, nil
}

// extractiveAnswer is ask_memory without a model: the top passages verbatim,
// numbered so the citation contract holds ([n] resolves to citations[n-1]
// exactly as in the synthesized form).
func (b *CoreBackends) extractiveAnswer(ctx context.Context, search MemorySearcher, query string) (domain.PublishedToolResult, error) {
	if search == nil {
		return domain.PublishedToolResult{}, errNotReady
	}
	results, err := search.Search(ctx, query, domain.PrincipalFromContext(ctx).ID)
	if err != nil {
		return domain.PublishedToolResult{}, fmt.Errorf("search: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("No synthesis model is configured on this kernel; these are the most relevant passages, verbatim.\n")
	cites := make([]citation, 0, extractivePassages)
	for _, r := range results {
		n := len(cites) + 1
		snippet := truncate(r.Document.Text, extractivePassageChars)
		fmt.Fprintf(&sb, "\n[%d] %s\n", n, snippet)
		cites = append(cites, citation{
			N: n, DocID: r.Document.ID, Snippet: snippet,
			SectionPath: r.Document.SectionPath, Score: r.Score, Tags: docTags(r.Document),
		})
		if len(cites) >= extractivePassages {
			break
		}
	}
	return domain.PublishedToolResult{
		Structured: map[string]any{
			"answer": sb.String(), "status": "extractive", "extractive": true, "citations": cites,
		},
		Text: sb.String(),
	}, nil
}

func (b *CoreBackends) getDocument(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	_, _, _, docs := b.snapshot()
	if docs == nil {
		return domain.PublishedToolResult{}, errNotReady
	}
	var in struct {
		DocID    string `json:"doc_id"`
		MaxChars int    `json:"max_chars"`
		Offset   int    `json:"offset"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	if strings.TrimSpace(in.DocID) == "" {
		return domain.PublishedToolResult{}, fmt.Errorf("doc_id is required")
	}
	maxChars := clamp(in.MaxChars, getDefaultChars, getMaxChars)
	if in.Offset < 0 {
		in.Offset = 0
	}

	doc, err := docs.GetDocument(ctx, domain.PrincipalFromContext(ctx), in.DocID)
	if err != nil {
		// ErrDocumentNotFound covers absent AND unreadable, by the port's own
		// contract — do not separate them here either.
		return domain.PublishedToolResult{}, err
	}

	// Character (rune) addressing, so a page boundary can never split a UTF-8
	// sequence and offsets mean the same thing for every script.
	runes := []rune(doc.Text)
	total := len(runes)
	start := min(in.Offset, total)
	end := min(start+maxChars, total)
	body := string(runes[start:end])

	return domain.PublishedToolResult{
		Structured: map[string]any{
			"doc_id":         doc.ID,
			"document_type":  doc.DocumentType,
			"text":           body,
			"offset":         start,
			"returned_chars": end - start,
			"total_chars":    total,
			"truncated":      end < total,
			"tags":           docTags(doc),
		},
		Text: body,
	}, nil
}

// listedDocument is one list_documents row — ids and labels, never bodies.
type listedDocument struct {
	ID              string   `json:"id"`
	Title           string   `json:"title,omitempty"`
	SourceType      string   `json:"source_type,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	ChunkCount      int      `json:"chunk_count"`
	CreatedAtUnixMs int64    `json:"created_at_unix_ms"`
}

func (b *CoreBackends) listDocuments(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	_, _, documents, _ := b.snapshot()
	if documents == nil {
		return domain.PublishedToolResult{}, errNotReady
	}
	var in struct {
		Limit          int      `json:"limit"`
		Cursor         string   `json:"cursor"`
		UnlabelledOnly bool     `json:"unlabelled_only"`
		IDPrefix       string   `json:"id_prefix"`
		Tags           []string `json:"tags"`
	}
	if err := parseArgs(args, &in); err != nil {
		return domain.PublishedToolResult{}, err
	}
	page, next, total, err := documents.ListDocuments(ctx, memory.DocumentFilter{
		Limit:          clamp(in.Limit, listDefaultLimit, listMaxLimit),
		Cursor:         in.Cursor,
		UnlabelledOnly: in.UnlabelledOnly,
		IDPrefix:       in.IDPrefix,
		Tags:           in.Tags,
	})
	if err != nil {
		return domain.PublishedToolResult{}, fmt.Errorf("list documents: %w", err)
	}
	rows := make([]listedDocument, 0, len(page))
	for _, d := range page {
		rows = append(rows, listedDocument{
			ID: d.ID, Title: d.Title, SourceType: d.SourceType, Tags: d.Tags,
			ChunkCount: d.ChunkCount, CreatedAtUnixMs: d.CreatedAt.UnixMilli(),
		})
	}
	return domain.PublishedToolResult{
		Structured: map[string]any{
			"documents": rows, "next_cursor": next, "total_matching": total,
		},
		Text: fmt.Sprintf("%d of %d document(s)", len(rows), total),
	}, nil
}

// ── small helpers ────────────────────────────────────────────────────────────

// parseArgs unmarshals the raw arguments; absent args mean "all defaults".
func parseArgs(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// clamp applies the default for an unset value and the cap for an excessive one.
func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// docTags reads the classification tags off a document's metadata, the same
// place the operator plane reads them.
func docTags(d domain.Document) []string {
	raw, ok := d.Metadata["tags"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
