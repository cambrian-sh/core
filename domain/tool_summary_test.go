package domain

import (
	"strings"
	"testing"
)

// toolSummary derives a short, deterministic Tier-1 one-liner (ADR-0045 D3):
// strip leading markdown → first sentence-or-line → word-boundary cap → name
// fallback. These are the cases the diffuse-embedding fix and the menu both rely on.
func TestToolSummary(t *testing.T) {
	longSentence := "Back up the configured database to a remote secure file transfer endpoint and verify the archive checksum before reporting success to the caller and operator"

	cases := []struct {
		name string
		tool SystemTool
		want string
	}{
		{
			name: "first sentence wins over the rest",
			tool: SystemTool{Name: "scrape", Description: "Scrape a URL and return markdown. Supports JS rendering, PDFs, and structured extraction."},
			want: "Scrape a URL and return markdown.",
		},
		{
			name: "newline terminates before a period",
			tool: SystemTool{Name: "x", Description: "Search the web\nLong second paragraph that should not appear."},
			want: "Search the web",
		},
		{
			name: "leading markdown header stripped",
			tool: SystemTool{Name: "x", Description: "# Search Tool\nSearches the web."},
			want: "Search Tool",
		},
		{
			name: "internal whitespace collapsed",
			tool: SystemTool{Name: "x", Description: "Does   a\tthing.  And more."},
			want: "Does a thing.",
		},
		{
			name: "over-cap truncates at a word boundary with ellipsis",
			tool: SystemTool{Name: "x", Description: longSentence},
			want: "Back up the configured database to a remote secure file transfer endpoint and verify the archive checksum before…",
		},
		{
			name: "empty description falls back to humanized name",
			tool: SystemTool{Name: "firecrawl_scrape", Description: ""},
			want: "firecrawl scrape",
		},
		{
			name: "whitespace-only description falls back to humanized name",
			tool: SystemTool{Name: "web_search", Description: "   \n\t  "},
			want: "web search",
		},
		{
			name: "mcp identity falls back to the trailing tool segment",
			tool: SystemTool{Name: "mcp:firecrawl/do_thing", Description: ""},
			want: "do thing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolSummary(tc.tool); got != tc.want {
				t.Errorf("toolSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ToolDisclosure renders Tier-1 (short summary + arg-names-only schema) by
// default and Tier-2 (raw description + full schema) when full=true (ADR-0045).
func TestToolDisclosure(t *testing.T) {
	tool := SystemTool{
		Name:        "scrape",
		Description: "Scrape a URL and return markdown. Supports JS rendering and PDFs.",
		Schema:      []byte(`{"properties":{"url":{"type":"string"},"formats":{"type":"array"}}}`),
	}

	// Tier-1: short summary, arg-names-only schema (no types).
	desc, schema := ToolDisclosure(tool, false)
	if desc != "Scrape a URL and return markdown." {
		t.Errorf("Tier-1 description should be the short summary, got %q", desc)
	}
	if strings.Contains(schema, "string") || strings.Contains(schema, "array") {
		t.Errorf("Tier-1 schema must drop types (arg names only), got %q", schema)
	}
	for _, name := range []string{"url", "formats"} {
		if !strings.Contains(schema, name) {
			t.Errorf("Tier-1 schema should keep arg name %q, got %q", name, schema)
		}
	}

	// Tier-2: raw description + full schema (types present).
	desc, schema = ToolDisclosure(tool, true)
	if desc != tool.Description {
		t.Errorf("Tier-2 description should be the full description, got %q", desc)
	}
	if !strings.Contains(schema, "string") {
		t.Errorf("Tier-2 schema should carry full types, got %q", schema)
	}
}

// An argless tool yields an empty Tier-1 schema (no args to show).
func TestToolDisclosure_NoArgs(t *testing.T) {
	if _, schema := ToolDisclosure(SystemTool{Name: "ping", Description: "Ping the service."}, false); schema != "" {
		t.Errorf("argless tool should yield empty Tier-1 schema, got %q", schema)
	}
}

// The over-cap result must never exceed the cap (plus the single ellipsis rune)
// and must not split a word.
func TestToolSummary_CapBound(t *testing.T) {
	got := toolSummary(SystemTool{Name: "x", Description: "word " + strings.Repeat("alpha ", 60)})
	// runes ≤ cap + 1 ellipsis; rough byte check is sufficient for ASCII here.
	if r := []rune(got); len(r) > toolSummaryMaxChars+1 {
		t.Errorf("summary exceeds cap: %d runes (%q)", len(r), got)
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("over-cap summary should end with an ellipsis, got %q", got)
	}
}
