package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// writeMCPConfig writes a minimal valid config carrying the given server block.
func writeMCPConfig(t *testing.T, serverBlock string) string {
	t.Helper()
	raw := `{
		"llm": {"endpoint":"http://localhost:11434","model":"llama3"},
		"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
		"server": ` + serverBlock + `
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// THE default this slice most needs pinned: the endpoint external agents dial is
// OFF unless a deployment said otherwise. A kernel upgrade must never be the
// reason a port opened.
func TestMCPEndpoint_DefaultsToDisabled(t *testing.T) {
	cfg, err := config.LoadConfig(writeMCPConfig(t, `{"port":"50051"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.MCP.Enabled {
		t.Fatal("server.mcp.enabled defaulted to TRUE — the MCP endpoint must open only when asked")
	}
	if cfg.Server.MCP.Port != 0 {
		t.Errorf("server.mcp.port = %d, want no implied default", cfg.Server.MCP.Port)
	}
	if cfg.Server.MCP.MaxConcurrentPerClient != config.DefaultMCPMaxConcurrentPerClient {
		t.Errorf("max_concurrent_per_client = %d, want the %d default — the cap is not opt-in",
			cfg.Server.MCP.MaxConcurrentPerClient, config.DefaultMCPMaxConcurrentPerClient)
	}
}

// A disabled endpoint is never validated: an unconfigured block on a kernel that
// will not serve it is not a mistake, and failing a boot over one would be.
func TestMCPEndpoint_DisabledBlockIsNotValidated(t *testing.T) {
	if _, err := config.LoadConfig(writeMCPConfig(t,
		`{"port":"50051","mcp":{"enabled":false,"port":0,"clients":[]}}`)); err != nil {
		t.Fatalf("a disabled endpoint with an empty block must not fail the boot: %v", err)
	}
}

func TestMCPEndpoint_EnabledRequiresPortAndClients(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
		want   string
	}{
		{
			name:   "no port",
			server: `{"port":"50051","mcp":{"enabled":true,"clients":["ci-bot"]}}`,
			want:   "server.mcp.port",
		},
		{
			name:   "port out of range",
			server: `{"port":"50051","mcp":{"enabled":true,"port":99999,"clients":["ci-bot"]}}`,
			want:   "server.mcp.port",
		},
		{
			// Its OWN listener (ADR-0126 D3) — sharing the gRPC port is not a
			// tighter deployment, it is a second bind that cannot succeed.
			name:   "port collides with the gRPC plane",
			server: `{"port":"50051","mcp":{"enabled":true,"port":50051,"clients":["ci-bot"]}}`,
			want:   "server.port",
		},
		{
			// The token is never optional (D4), so a client roster is not
			// optional either: an endpoint with no named client serves nobody.
			name:   "no clients",
			server: `{"port":"50051","mcp":{"enabled":true,"port":50052}}`,
			want:   "server.mcp.clients",
		},
		{
			name:   "duplicate client",
			server: `{"port":"50051","mcp":{"enabled":true,"port":50052,"clients":["ci-bot","ci-bot"]}}`,
			want:   `lists "ci-bot" twice`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadConfig(writeMCPConfig(t, tc.server))
			if err == nil {
				t.Fatalf("accepted an unservable endpoint config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %q: %v", tc.want, err)
			}
		})
	}
}

func TestMCPEndpoint_EnabledAndWellFormedLoads(t *testing.T) {
	cfg, err := config.LoadConfig(writeMCPConfig(t,
		`{"port":"50051","mcp":{"enabled":true,"port":50052,"clients":["ci-bot","laptop"],"max_concurrent_per_client":2}}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	m := cfg.Server.MCP
	if !m.Enabled || m.Port != 50052 || len(m.Clients) != 2 || m.MaxConcurrentPerClient != 2 {
		t.Fatalf("server.mcp round-trip = %+v", m)
	}
}
