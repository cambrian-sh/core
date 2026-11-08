package config

import (
	"strings"
	"testing"
)

// Precedence of mcp.json vs the store (integrations surface): a stored entry
// replaces the file entry with its id, a tombstone hides one, new ids append,
// file-only servers stay visible, and an env var pinning the whole list wins.

const mcpFile = `{"mcp": {"servers": [
  {"id": "fs",  "transport": "stdio", "endpoint": "fs-file"},
  {"id": "git", "transport": "stdio", "endpoint": "git-file"},
  {"id": "old", "transport": "stdio", "endpoint": "old-file"}]}}`

func serverIDs(cfg *Config) map[string]MCPServerConfig {
	out := map[string]MCPServerConfig{}
	for _, s := range cfg.MCP.Servers {
		out[s.ID] = s
	}
	return out
}

func TestMCPStore_MergesByIDOverMCPJSON(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "mcp.json", mcpFile)
	off := false
	store := mapStore{
		MCPServerOverrideKey("fs"):    StoredMCPServer{MCPServerConfig: MCPServerConfig{ID: "fs", Transport: "stdio", Endpoint: "fs-store", Enabled: &off}},
		MCPServerOverrideKey("old"):   StoredMCPServer{MCPServerConfig: MCPServerConfig{ID: "old"}, Removed: true},
		MCPServerOverrideKey("brave"): StoredMCPServer{MCPServerConfig: MCPServerConfig{ID: "brave", Transport: "stdio", Endpoint: "npx", CatalogID: "brave-search", SecretEnv: []string{"BRAVE_API_KEY"}}},
	}
	cfg, prov := loadFrom(t, dir, store)
	got := serverIDs(cfg)
	if len(got) != 3 {
		t.Fatalf("want fs, git, brave; got %+v", cfg.MCP.Servers)
	}
	if got["fs"].Endpoint != "fs-store" || got["fs"].IsEnabled() {
		t.Errorf("store entry must replace the file entry: %+v", got["fs"])
	}
	if got["git"].Endpoint != "git-file" || !got["git"].IsEnabled() {
		t.Errorf("file-only server must stay visible and enabled by default: %+v", got["git"])
	}
	if _, ok := got["old"]; ok {
		t.Error("tombstoned server still configured")
	}
	if got["brave"].CatalogID != "brave-search" {
		t.Errorf("store-only server not appended: %+v", got["brave"])
	}
	if prov.Source(MCPServersKey) != SourceStore {
		t.Errorf("provenance = %q", prov.Source(MCPServersKey))
	}
}

func TestMCPStore_LegacyWholeListMigratesPerID(t *testing.T) {
	store := mapStore{MCPServersKey: []any{
		map[string]any{"id": "a", "transport": "http", "endpoint": "http://a"},
		map[string]any{"id": "b", "transport": "http", "endpoint": "http://b"},
	}}
	ids, err := MigrateStoredMCPServers(store)
	if err != nil || strings.Join(ids, ",") != "a,b" {
		t.Fatalf("migrate: %v %v", ids, err)
	}
	if _, ok := store[MCPServersKey]; ok {
		t.Fatal("legacy key not deleted")
	}
	if _, ok := store[MCPServerOverrideKey("a")]; !ok {
		t.Fatal("per-id entry not written")
	}
	if ids, _ := MigrateStoredMCPServers(store); len(ids) != 0 {
		t.Fatal("migration not idempotent")
	}
	// Semantics change on purpose: a file server the old list did not name is
	// visible again after the migration.
	dir := baseBundle(t)
	writeCfg(t, dir, "mcp.json", mcpFile)
	cfg, _ := loadFrom(t, dir, store)
	if len(cfg.MCP.Servers) != 5 {
		t.Fatalf("want 3 file + 2 migrated, got %d", len(cfg.MCP.Servers))
	}
}

func TestMCPServerValidation(t *testing.T) {
	ok := MCPServerConfig{ID: "x", EnvValues: map[string]string{"SANDBOX_NETWORK_MODE": "none"}, SecretEnv: []string{"X_API_KEY"}}
	if errs := ValidateMCPServer(ok); len(errs) != 0 {
		t.Fatalf("valid server rejected: %v", errs)
	}
	for name, s := range map[string]MCPServerConfig{
		"credential as literal": {ID: "x", EnvValues: map[string]string{"X_API_KEY": "sk"}},
		"kernel literal":        {ID: "x", EnvValues: map[string]string{"CAMBRIAN_SECRET_KEY": "k"}},
		"kernel secret":         {ID: "x", SecretEnv: []string{"LANGFUSE_SECRET_KEY"}},
		"bad name":              {ID: "x", EnvValues: map[string]string{"A=B": "c"}},
		"both literal & secret": {ID: "x", EnvValues: map[string]string{"MODE": "a"}, SecretEnv: []string{"MODE"}},
	} {
		if errs := ValidateMCPServer(s); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	// And it is a BOOT error for a file, not a silently-ignored value.
	dir := baseBundle(t)
	writeCfg(t, dir, "mcp.json", `{"mcp": {"servers": [{"id": "x", "transport": "stdio", "endpoint": "e", "env_values": {"MY_TOKEN": "t"}}]}}`)
	if _, _, err := LoadConfigWithStore(dir+"/config.json", nil); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("want a boot validation error, got %v", err)
	}
}
