package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261106-listmcpservers-tool-count-is-always-0: MCP tools are registered as
// mcp:<server-id>/<tool> (connector.discover), and the count must be taken on
// that shape — not on a "<id>." / "<id>:" prefix, which never matched and made
// ListMCPServers report 0 tools for every server.
func TestCountMCPTools_CountsByParsedServerID(t *testing.T) {
	tools := []domain.SystemTool{
		{Name: "mcp:fetch/fetch"},
		{Name: "mcp:pdf-reader/read_pdf"},
		{Name: "mcp:pdf-reader/search_pdf"},
		{Name: "mcp:pdf-reader-2/read_pdf"}, // a server whose id extends another's
		{Name: "pdf-reader:native"},         // not an MCP tool
		{Name: "pdf-reader.native"},         // not an MCP tool
		{Name: "mcp:pdf-reader"},            // malformed: no tool part
		{Name: "system_shell"},
	}
	cases := map[string]int{"fetch": 1, "pdf-reader": 2, "pdf-reader-2": 1, "time": 0}
	for id, want := range cases {
		if got := countMCPTools(tools, id); got != want {
			t.Errorf("countMCPTools(%q) = %d, want %d", id, got, want)
		}
	}
}
