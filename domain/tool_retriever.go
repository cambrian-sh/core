package domain

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// ToolRetriever ranks an agent's granted tools by relevance to a query and
// returns the top-k tool names (ADR-0044). It is the tool-domain PORT behind
// which relevance ranking lives — the ToolExecutor depends on this, never on the
// vector store. Implemented by KeywordToolRetriever (fake/tracer) and the
// pgvector-backed VectorToolRetriever (infrastructure adapter).
//
// grantedNames is the already-authorized candidate set (AvailableTools); the
// retriever ranks within it and may apply a relevance floor (returning fewer than
// k, or none, when nothing is relevant).
type ToolRetriever interface {
	Rank(ctx context.Context, query string, grantedNames []string, k int) ([]string, error)
}

// BuildToolDoc assembles the deterministic embedding document for a tool
// (ADR-0044 D5, ADR-0045 D2): name + the Tier-1 short summary + arg names,
// prefixed for nomic-embed-text's asymmetric document side. Pure and
// deterministic — no LLM. ADR-0045 replaced the full description with
// toolSummary so the embedded vector is sharp (a 2k-token description averages
// into a diffuse vector that mis-ranks) AND matches the served Tier-1 menu line
// by construction (same deriver, both places). This is the seam where optional
// LLM enrichment plugs in later.
func BuildToolDoc(t SystemTool) string {
	var b strings.Builder
	b.WriteString("search_document: ")
	b.WriteString(t.Name)
	if s := toolSummary(t); s != "" {
		b.WriteString(". ")
		b.WriteString(s)
	}
	if args := toolArgNames(t.Schema); len(args) > 0 {
		b.WriteString(". args: ")
		b.WriteString(strings.Join(args, ", "))
	}
	return b.String()
}

// toolSummaryMaxChars caps the Tier-1 one-liner (~30 tokens). A named constant,
// tunable by measurement — not a config knob (ADR-0045 D3).
const toolSummaryMaxChars = 120

// toolSummary derives a tool's deterministic Tier-1 one-line capability summary
// (ADR-0045 D2/D3): strip leading markdown → first sentence-or-line → cap at a
// word boundary → humanized-name fallback when the description is empty. Pure,
// no LLM. It is the SINGLE source for both the embedded doc (BuildToolDoc) and
// the served menu line, so the vector that ranks is the text the agent sees.
func toolSummary(t SystemTool) string { return deriveSummary(t.Name, t.Description) }

// deriveSummary is the shared deterministic one-liner derivation (ADR-0045 D3),
// reused for skills (ADR-0046): strip leading markdown → first sentence-or-line →
// word-boundary cap → humanized-name fallback. Pure, no LLM.
func deriveSummary(name, description string) string {
	seg := firstSentenceOrLine(stripLeadingMarkup(description))
	seg = capAtWordBoundary(collapseSpaces(seg), toolSummaryMaxChars)
	if seg == "" {
		return humanizeName(name)
	}
	return seg
}

// stripLeadingMarkup trims the markdown lead-in common to MCP descriptions
// (headers, bullets, code fences, blockquotes) so the first real sentence is
// what gets summarized. It deliberately omits '-'/'_' from the cutset to avoid
// eating hyphenated/underscored content.
func stripLeadingMarkup(s string) string {
	return strings.TrimLeft(s, "#*`> \t\r\n")
}

// firstSentenceOrLine returns the first non-empty segment terminated by a
// newline, a sentence period (". "), or end-of-string.
func firstSentenceOrLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1] // include the period
	}
	return strings.TrimSpace(s)
}

// collapseSpaces normalizes internal whitespace runs to single spaces.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// capAtWordBoundary truncates to at most max runes, backing up to the last word
// boundary and appending an ellipsis. Rune-aware so it never splits a UTF-8
// codepoint.
func capAtWordBoundary(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := string([]rune(s)[:max])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ") + "…"
}

// humanizeName derives a readable summary from a tool's identity when it has no
// description: the trailing segment of an mcp:<server>/<tool> id, with
// separators turned to spaces (e.g. "firecrawl_scrape" → "firecrawl scrape").
func humanizeName(name string) string {
	if i := strings.LastIndexAny(name, "/:"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	return collapseSpaces(strings.NewReplacer("_", " ", "-", " ").Replace(name))
}

// ToolDisclosure renders a tool's disclosure fields for the served menu
// (ADR-0045 D1/D5). full=false (Tier-1, the always-served menu): the short
// summary + an arg-names-only schema. full=true (Tier-2, describe_tool): the
// raw description + full schema. Pure — the network layer maps the result onto
// the wire ToolDescriptor. Tier-1 reuses the SAME toolSummary as the embedding
// (D2), so the menu line matches the vector that ranked it.
func ToolDisclosure(t SystemTool, full bool) (description, schemaJSON string) {
	if full {
		return t.Description, string(t.Schema)
	}
	return toolSummary(t), argNamesSchemaJSON(t.Schema)
}

// argNamesSchemaJSON reduces a full JSON-Schema to an arg-names-only schema
// ({"properties":{"name":{}}}), reusing toolArgNames. Empty/argless ⇒ "" so the
// menu entry simply carries no args. Deterministic (sorted keys).
func argNamesSchemaJSON(schema []byte) string {
	names := toolArgNames(schema)
	if len(names) == 0 {
		return ""
	}
	props := make(map[string]map[string]any, len(names))
	for _, n := range names {
		props[n] = map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"properties": props})
	if err != nil {
		return ""
	}
	return string(b)
}

// toolArgNames extracts the property names from a tool's JSON-Schema. Tolerates
// an absent/invalid schema (returns nil).
func toolArgNames(schema []byte) []string {
	if len(schema) == 0 {
		return nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	names := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic order
	return names
}

// KeywordToolRetriever is a dependency-free ToolRetriever: it ranks the granted
// tools by keyword overlap between the query and each tool's name+description.
// Used as the tracer/fake before the pgvector adapter (0044-01); also a fallback
// when no embedder is available. Deterministic.
type KeywordToolRetriever struct {
	Registry ToolRegistry
}

// Rank implements ToolRetriever.
func (r KeywordToolRetriever) Rank(_ context.Context, query string, grantedNames []string, k int) ([]string, error) {
	words := tokenizeQuery(query)
	type scored struct {
		name  string
		score int
	}
	ranked := make([]scored, 0, len(grantedNames))
	for _, n := range grantedNames {
		t, ok := r.Registry.Get(n)
		if !ok {
			continue
		}
		doc := strings.ToLower(t.Name + " " + t.Description)
		score := 0
		for w := range words {
			if strings.Contains(doc, w) {
				score++
			}
		}
		ranked = append(ranked, scored{n, score})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	out := make([]string, 0, k)
	for i := 0; i < len(ranked) && i < k; i++ {
		out = append(out, ranked[i].name)
	}
	return out, nil
}

func tokenizeQuery(q string) map[string]struct{} {
	words := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) > 2 {
			words[w] = struct{}{}
		}
	}
	return words
}
