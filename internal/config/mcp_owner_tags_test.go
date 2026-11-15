package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ENG-1046 (owner decision 2026-11-15, ADR-0146 D5): owner tags are
// kernel-stamped, so an MCP server's classification tags — server-level,
// per-tool or the default tool policy — may not carry one.
func TestValidateMCPServer_RefusesOwnerTags(t *testing.T) {
	clean := MCPServerConfig{ID: "crm", Transport: "http", Endpoint: "https://crm.example/mcp",
		ClassificationTags: []string{"crm"}, Tools: []MCPToolConfig{{Name: "lookup", ClassificationTags: []string{"crm"}}}}
	if errs := ValidateMCPServer(clean); len(errs) != 0 {
		t.Fatalf("clean server refused: %v", errs)
	}
	cases := map[string]func(*MCPServerConfig){
		"mcp.servers[crm].classification_tags": func(s *MCPServerConfig) { s.ClassificationTags = []string{"crm", "owner:alice"} },
		"mcp.servers[crm].tools[lookup].classification_tags": func(s *MCPServerConfig) {
			s.Tools = []MCPToolConfig{{Name: "lookup", ClassificationTags: []string{"owner:alice"}}}
		},
		"mcp.servers[crm].default_tool_policy.classification_tags": func(s *MCPServerConfig) {
			s.DefaultToolPolicy = &MCPToolConfig{ClassificationTags: []string{"owner:alice"}}
		},
	}
	for field, mut := range cases {
		s := clean
		s.Tools = append([]MCPToolConfig(nil), clean.Tools...)
		mut(&s)
		errs := ValidateMCPServer(s)
		if len(errs) != 1 || !strings.Contains(errs[0], field) || !strings.Contains(errs[0], `"owner:alice"`) {
			t.Fatalf("%s: want one error naming the field and tag, got %v", field, errs)
		}
	}
}

// The boot path: an mcp.json whose server carries an owner tag fails the
// config load, naming the server (as every other invalid mcp entry does).
func TestLoadConfig_MCPJSONOwnerTagFailsBoot(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", `{"database":{"host":"h","user":"u","password":"p","dbname":"d"}}`)
	write("mcp.json", `{"mcp":{"servers":[{"id":"crm","transport":"http","endpoint":"https://crm.example/mcp","classification_tags":["owner:alice"]}]}}`)
	_, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
	if err == nil || !strings.Contains(err.Error(), "mcp.servers[crm].classification_tags") || !strings.Contains(err.Error(), "owner:alice") {
		t.Fatalf("want a load error naming the server and tag, got %v", err)
	}
	write("mcp.json", `{"mcp":{"servers":[{"id":"crm","transport":"http","endpoint":"https://crm.example/mcp","classification_tags":["crm"]}]}}`)
	if _, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil); err != nil {
		t.Fatalf("clean mcp.json must load: %v", err)
	}
}
