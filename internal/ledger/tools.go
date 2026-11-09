package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// Tool names. Kernel-native (domain.KernelTool): served in process by the
// ToolExecutor through ExecuteTool, so every agent reaches the ledger through the
// kernel (decisions/all-agent-communication-passes-through-the-kernel.md) and the
// proto surface does not change.
const (
	ToolGet  = "ledger_get"
	ToolFind = "ledger_find"
)

// Tools returns the two ledger tools over the ENFORCED reader. pageBytes bounds
// one page of a text body (execution.ledger.page_bytes; <= 0 ⇒ 16000).
func Tools(reader domain.LedgerReader, pageBytes int) []domain.KernelTool {
	if pageBytes <= 0 {
		pageBytes = 16000
	}
	return []domain.KernelTool{getTool{reader: reader, page: pageBytes}, findTool{reader: reader}}
}

type getTool struct {
	reader domain.LedgerReader
	page   int
}

func (getTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolGet,
		Description: "Read a ledger entry (a tool result or step output recorded by the kernel this conversation) " +
			"by its id (led_...). Optional: path selects part of a JSON body (a.b[0].c or /a/b/0); rows " +
			"selects a slice of a JSON list (\"10:20\"); page reads a long text body page by page (1-based). " +
			"Use the exact figures, ids and dates it returns.",
		Schema: []byte(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"ledger entry id, led_..."},` +
			`"path":{"type":"string","description":"part of a JSON body: a.b[0].c or /a/b/0"},` +
			`"rows":{"type":"string","description":"slice of a JSON list, start:end (end exclusive)"},` +
			`"page":{"type":"integer","minimum":1,"description":"page of a long body, 1-based"}},` +
			`"required":["id"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

type getArgs struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Rows string `json:"rows"`
	Page int    `json:"page"`
}

func (t getTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a getArgs
	if err := json.Unmarshal(orEmpty(call.ArgsJSON), &a); err != nil {
		return nil, fmt.Errorf("ledger_get: arguments are not a JSON object: %v", err)
	}
	a.ID = strings.TrimSpace(a.ID)
	if a.ID == "" {
		return nil, fmt.Errorf("ledger_get: id is required")
	}
	e, err := t.reader.Get(ctx, a.ID)
	if err != nil {
		return nil, fmt.Errorf("ledger_get: %v", err)
	}
	if e == nil {
		// Absent and forbidden read the same (authz.EnforcingLedger).
		return json.Marshal(map[string]any{"found": false, "id": a.ID})
	}
	out := map[string]any{
		"found": true, "id": e.ID, "kind": e.Kind, "step": e.StepIndex, "run_id": e.RunID,
		"content_type": e.ContentType, "size_bytes": e.SizeBytes,
	}
	if e.ToolName != "" {
		out["tool"] = e.ToolName
	}
	if e.Truncated {
		out["truncated"] = true
		out["note"] = fmt.Sprintf("only the first %d of %d bytes were kept", len(e.Body), e.SizeBytes)
	}
	var text string
	var value any
	isJSON := json.Unmarshal(e.Body, &value) == nil
	switch {
	case isJSON:
		if a.Path != "" {
			v, perr := SelectPath(value, a.Path)
			if perr != nil {
				return nil, fmt.Errorf("ledger_get: path %q: %v", a.Path, perr)
			}
			value = v
			out["path"] = a.Path
		}
		if a.Rows != "" {
			v, total, rerr := selectRows(value, a.Rows)
			if rerr != nil {
				return nil, fmt.Errorf("ledger_get: rows %q: %v", a.Rows, rerr)
			}
			value = v
			out["rows"] = a.Rows
			out["total_rows"] = total
		}
		b, _ := json.Marshal(value)
		if len(b) <= t.page && a.Page <= 1 {
			out["value"] = json.RawMessage(b)
			return json.Marshal(out)
		}
		text = string(b)
	default:
		if a.Path != "" || a.Rows != "" {
			return nil, fmt.Errorf("ledger_get: path and rows need a JSON body; this entry is %s", e.ContentType)
		}
		text = string(e.Body)
	}
	page, pages := pageOf(text, t.page, a.Page)
	if a.Page > pages && pages > 0 {
		return nil, fmt.Errorf("ledger_get: page %d of %d", a.Page, pages)
	}
	out["text"] = page
	out["page"] = max(a.Page, 1)
	out["pages"] = pages
	return json.Marshal(out)
}

type findTool struct{ reader domain.LedgerReader }

func (findTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolFind,
		Description: "Search the ledger: the tool results, step outputs and calculations the kernel recorded. " +
			"scope is run (this plan), turn (this conversation turn) or conversation. Filter by step, tool, " +
			"kind (tool_result, step_output, computed, artifact, scratch) or text. Returns ids and previews; " +
			"read an entry with ledger_get.",
		Schema: []byte(`{"type":"object","properties":{` +
			`"scope":{"type":"string","enum":["run","turn","conversation"]},` +
			`"step":{"type":"integer"},"tool":{"type":"string"},` +
			`"kind":{"type":"string","enum":["tool_result","step_output","computed","artifact","scratch"]},` +
			`"text":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100}},` +
			`"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

type findArgs struct {
	Scope string `json:"scope"`
	Step  *int   `json:"step"`
	Tool  string `json:"tool"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Limit int    `json:"limit"`
}

// findDefaultLimit / findMaxLimit bound one ledger_find answer.
const (
	findDefaultLimit = 30
	findMaxLimit     = 100
)

func (t findTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a findArgs
	if err := json.Unmarshal(orEmpty(call.ArgsJSON), &a); err != nil {
		return nil, fmt.Errorf("ledger_find: arguments are not a JSON object: %v", err)
	}
	// The scope resolves from the CALLER's lease-resolved provenance, never from an
	// argument: an agent can search its own run, turn or conversation only.
	p := domain.LedgerProvenanceFromContext(ctx)
	scope := strings.TrimSpace(a.Scope)
	if scope == "" {
		switch {
		case p.RunID != "":
			scope = "run"
		case p.TurnID != "":
			scope = "turn"
		default:
			scope = "conversation"
		}
	}
	q := domain.LedgerQuery{ToolName: a.Tool, Text: a.Text}
	switch scope {
	case "run":
		q.RunID = p.RunID
	case "turn":
		q.TurnID = p.TurnID
	case "conversation":
		q.ConversationID = p.ConversationID
	default:
		return nil, fmt.Errorf("ledger_find: scope must be run, turn or conversation")
	}
	if !q.Scoped() {
		return nil, fmt.Errorf("ledger_find: this call has no %s; try another scope", scope)
	}
	if a.Kind != "" {
		if !domain.ValidLedgerKind(domain.LedgerKind(a.Kind)) {
			return nil, fmt.Errorf("ledger_find: unknown kind %q", a.Kind)
		}
		q.Kind = domain.LedgerKind(a.Kind)
	}
	if a.Step != nil {
		q.StepIndex, q.HasStep = *a.Step, true
	}
	q.Limit = a.Limit
	if q.Limit <= 0 {
		q.Limit = findDefaultLimit
	}
	if q.Limit > findMaxLimit {
		q.Limit = findMaxLimit
	}
	rows, err := t.reader.Find(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ledger_find: %v", err)
	}
	type item struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Tool      string `json:"tool,omitempty"`
		Step      int    `json:"step"`
		RunID     string `json:"run_id,omitempty"`
		Preview   string `json:"preview"`
		SizeBytes int64  `json:"size_bytes"`
		Truncated bool   `json:"truncated,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	items := make([]item, 0, len(rows))
	for _, e := range rows {
		items = append(items, item{ID: e.ID, Kind: string(e.Kind), Tool: e.ToolName, Step: e.StepIndex,
			RunID: e.RunID, Preview: e.Preview, SizeBytes: e.SizeBytes, Truncated: e.Truncated,
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	return json.Marshal(map[string]any{"scope": scope, "count": len(items), "entries": items})
}

func orEmpty(b []byte) []byte {
	if len(strings.TrimSpace(string(b))) == 0 {
		return []byte("{}")
	}
	return b
}

// SelectPath walks a decoded JSON value: dotted keys with [i] indexes
// ("a.b[0].c", "a.b.0.c") or a JSON Pointer ("/a/b/0"). Shared by ledger_get and
// the calculation tools (internal/calc), so a path means the same in both.
func SelectPath(v any, path string) (any, error) {
	var parts []string
	if strings.HasPrefix(path, "/") {
		for _, p := range strings.Split(path[1:], "/") {
			parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~"))
		}
	} else {
		norm := strings.NewReplacer("[", ".", "]", "").Replace(path)
		for _, p := range strings.Split(norm, ".") {
			if p != "" {
				parts = append(parts, p)
			}
		}
	}
	cur := v
	for _, p := range parts {
		// A string that is itself JSON (an MCP result's content[0].text) is walked
		// into, so a path can reach a record inside a tool's text payload.
		if s, isStr := cur.(string); isStr {
			var inner any
			if json.Unmarshal([]byte(s), &inner) == nil {
				cur = inner
			}
		}
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[p]
			if !ok {
				return nil, fmt.Errorf("no key %q", p)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(t) {
				return nil, fmt.Errorf("index %q out of range (%d items)", p, len(t))
			}
			cur = t[i]
		default:
			return nil, fmt.Errorf("%q: not an object or list", p)
		}
	}
	return cur, nil
}

// selectRows slices a JSON list: "start:end" (end exclusive, either side may be
// empty) or a single index "n".
func selectRows(v any, rows string) (any, int, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, 0, fmt.Errorf("the selected value is not a list")
	}
	n := len(arr)
	start, end := 0, n
	if i := strings.Index(rows, ":"); i >= 0 {
		var err error
		if s := strings.TrimSpace(rows[:i]); s != "" {
			if start, err = strconv.Atoi(s); err != nil {
				return nil, n, fmt.Errorf("bad start")
			}
		}
		if s := strings.TrimSpace(rows[i+1:]); s != "" {
			if end, err = strconv.Atoi(s); err != nil {
				return nil, n, fmt.Errorf("bad end")
			}
		}
	} else {
		i, err := strconv.Atoi(strings.TrimSpace(rows))
		if err != nil {
			return nil, n, fmt.Errorf("want start:end or an index")
		}
		start, end = i, i+1
	}
	start = min(max(start, 0), n)
	end = min(max(end, start), n)
	return arr[start:end], n, nil
}

// pageOf returns page (1-based) of text in pages of at most size bytes, cut at
// rune boundaries, and the page count.
func pageOf(text string, size, page int) (string, int) {
	if page < 1 {
		page = 1
	}
	var pages []string
	for len(text) > 0 {
		cut := min(size, len(text))
		for cut < len(text) && cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = min(size, len(text))
		}
		pages = append(pages, text[:cut])
		text = text[cut:]
	}
	if len(pages) == 0 {
		return "", 0
	}
	if page > len(pages) {
		return "", len(pages)
	}
	return pages[page-1], len(pages)
}
