package postgres

import (
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"

	"github.com/cambrian-sh/core/domain"
)

// The typed SearchFilter must render VALUES as bind parameters, never as SQL
// text. This is the property the old raw `goqu.L(opts.Filter)` path lacked: its
// producers hand-escaped quotes and a third caller could have skipped that.
func TestFilterExpressions_ValuesAreBindParameters(t *testing.T) {
	t.Parallel()
	payload := `x' OR '1'='1`
	f := domain.SearchFilter{
		IDs:      []string{"a", payload},
		Metadata: map[string]string{"source_hash": payload, "agent_id": payload},
	}
	exprs, fargs := filterExpressions(f, "")
	ds := dialect.From("chunks").Select("id")
	for _, e := range exprs {
		ds = ds.Where(e)
	}
	sql, dsArgs, err := ds.ToSQL()
	if err != nil {
		t.Fatal(err)
	}
	args, err := bindFilterArgs(dsArgs, fargs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, payload) || strings.Contains(sql, "agent_id") || strings.Contains(sql, "source_hash") {
		t.Fatalf("filter value or key leaked into SQL text: %s", sql)
	}
	// Keys are sorted, so the shape (and the statement-cache key) is stable.
	for _, frag := range []string{"id = ANY($1::text[])", "metadata ->> $2::text = $3::text", "metadata ->> $4::text = $5::text"} {
		if !strings.Contains(sql, frag) {
			t.Errorf("SQL %q missing %q", sql, frag)
		}
	}
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 bind parameters", args)
	}
	if ids, ok := args[0].([]string); !ok || len(ids) != 2 || ids[1] != payload {
		t.Errorf("ids arg = %#v", args[0])
	}
	if args[1] != "agent_id" || args[2] != payload || args[3] != "source_hash" || args[4] != payload {
		t.Errorf("metadata args out of order: %#v", args[1:])
	}
}

func TestFilterExpressions_ZeroAddsNothing(t *testing.T) {
	t.Parallel()
	exprs, args := filterExpressions(domain.SearchFilter{}, "")
	if exprs != nil || args != nil {
		t.Fatalf("zero filter produced %v / %v", exprs, args)
	}
	// An empty (non-nil) id list is "no restriction", not "match nothing" — the
	// tools_unrestricted contract of VectorToolRetriever.
	exprs, _ = filterExpressions(domain.SearchFilter{IDs: []string{}, Metadata: map[string]string{}}, "")
	if exprs != nil {
		t.Fatalf("empty filter produced %v", exprs)
	}
}

func TestFilterExpressions_Qualified(t *testing.T) {
	t.Parallel()
	exprs, _ := filterExpressions(domain.SearchFilter{IDs: []string{"a"}, Metadata: map[string]string{"k": "v"}}, TableChunks+".")
	ds := dialect.From(TableChunks).Select("id")
	for _, e := range exprs {
		ds = ds.Where(e)
	}
	sql, _, _ := ds.ToSQL()
	if !strings.Contains(sql, "chunks.id = ANY(") || !strings.Contains(sql, "chunks.metadata ->> ") {
		t.Fatalf("qualifier not applied: %s", sql)
	}
}

// The $n numbering assumes a non-prepared dataset contributes no args. If one
// ever does, the query must be refused rather than run with shuffled parameters.
func TestBindFilterArgs_RefusesPreparedDataset(t *testing.T) {
	t.Parallel()
	if _, err := bindFilterArgs([]any{"x"}, []any{"y"}); err == nil {
		t.Fatal("expected a refusal when the dataset already has bind args")
	}
	got, err := bindFilterArgs([]any{"x"}, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("no filter args must pass dataset args through: %v %v", got, err)
	}
	// And the premise: the adapter's dialect in default mode returns no args
	// even for `?`-placeholder literals, and leaves `$n` text alone.
	sql, args, err := dialect.From("t").Where(goqu.L("a = ?", "v"), goqu.L("b = $1")).ToSQL()
	if err != nil || len(args) != 0 || !strings.Contains(sql, "b = $1") {
		t.Fatalf("non-prepared premise broken: sql=%q args=%v err=%v", sql, args, err)
	}
}

// End to end against a real Postgres: injection-shaped values in every filter
// slot are inert data. Under the old raw-string path an unescaped producer
// passing `x' OR '1'='1` would have returned every row.
func TestPgSearchFilter_InjectionValuesAreInert(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()
	ctx := testCtx()

	for _, d := range []struct{ id, agent, text string }{
		{"test-sf-a", "agent-a", "alpha filter document"},
		{"test-sf-b", "agent-b", "beta filter document"},
		{"test-sf-q", `o'brien\`, "quoted filter document"},
	} {
		if err := adapter.Save(ctx, &domain.Document{
			ID: d.id, Text: d.text, DocumentType: domain.DocTypeMnemonicFact,
			Embedding: dummyEmb(), Metadata: map[string]any{"agent_id": d.agent},
		}); err != nil {
			t.Fatalf("save %s: %v", d.id, err)
		}
	}

	search := func(f domain.SearchFilter) map[string]bool {
		t.Helper()
		res, err := adapter.Search(ctx, dummyVec(), domain.SearchOptions{
			DocumentType: domain.DocTypeMnemonicFact, TopK: 10, Filter: f,
		})
		if err != nil {
			t.Fatalf("search %+v: %v", f, err)
		}
		got := map[string]bool{}
		for _, r := range res {
			if strings.HasPrefix(r.Document.ID, "test-sf-") {
				got[r.Document.ID] = true
			}
		}
		return got
	}

	if got := search(domain.SearchFilter{Metadata: map[string]string{"agent_id": `x' OR '1'='1`}}); len(got) != 0 {
		t.Errorf("metadata value injection returned rows: %v", got)
	}
	if got := search(domain.SearchFilter{Metadata: map[string]string{`agent_id' = 'agent-a' OR '1'='1`: "x"}}); len(got) != 0 {
		t.Errorf("metadata key injection returned rows: %v", got)
	}
	if got := search(domain.SearchFilter{IDs: []string{"test-sf-a", `x') OR ('1'='1`}}); len(got) != 1 || !got["test-sf-a"] {
		t.Errorf("id injection: got %v, want only test-sf-a", got)
	}
	// Quotes and a trailing backslash match exactly as data (no escape mode games).
	if got := search(domain.SearchFilter{Metadata: map[string]string{"agent_id": `o'brien\`}}); len(got) != 1 || !got["test-sf-q"] {
		t.Errorf("quoted value: got %v, want only test-sf-q", got)
	}
	// Positive control: the filter does select.
	if got := search(domain.SearchFilter{Metadata: map[string]string{"agent_id": "agent-b"}}); len(got) != 1 || !got["test-sf-b"] {
		t.Errorf("metadata equality: got %v, want only test-sf-b", got)
	}

	// The lexical half of hybrid retrieval applies the same filter.
	lex, err := adapter.LexicalSearch(ctx, "filter document", domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact, TopK: 10,
		Filter: domain.SearchFilter{IDs: []string{"test-sf-b", `' OR 1=1 --`}},
	})
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	if len(lex) != 1 || lex[0].Document.ID != "test-sf-b" {
		ids := make([]string, 0, len(lex))
		for _, r := range lex {
			ids = append(ids, r.Document.ID)
		}
		t.Errorf("lexical filter: got %v, want only test-sf-b", ids)
	}
}
