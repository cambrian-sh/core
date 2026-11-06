package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func writeA2AConfig(t *testing.T, serverBlock, a2aBlock string) string {
	t.Helper()
	raw := `{
		"llm": {"endpoint":"http://localhost:11434","model":"llama3"},
		"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
		"server": ` + serverBlock
	if a2aBlock != "" {
		raw += `, "a2a": ` + a2aBlock
	}
	raw += "}"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ADR-0139 D4 defaults: the card listener is OFF with no implied port, no
// remote agents are registered, and remote signatures are checked when they
// are by a trusted key (optional).
func TestA2A_Defaults(t *testing.T) {
	cfg, err := config.LoadConfig(writeA2AConfig(t, `{"port":"50051"}`, ""))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	a := cfg.Server.A2A
	if a.Enabled || a.Port != 0 || a.IncludeSystemAgents || len(a.Interfaces) != 0 || a.SigningKeyFile != "" {
		t.Fatalf("server.a2a defaults = %+v, want disabled and empty", a)
	}
	if cfg.A2A.SignaturePolicy != config.DefaultA2ASignaturePolicy || config.DefaultA2ASignaturePolicy != "optional" {
		t.Errorf("a2a.signature_policy = %q, want optional", cfg.A2A.SignaturePolicy)
	}
	if len(cfg.A2A.Endpoints) != 0 || cfg.A2A.TrustedJWKSFile != "" {
		t.Errorf("a2a defaults = %+v", cfg.A2A)
	}
}

func TestA2A_Validation(t *testing.T) {
	for _, tc := range []struct {
		name, server, a2a, wantErr string
	}{
		{"disabled block not validated", `{"port":"50051","a2a":{"enabled":false,"port":0,"provider_url":"x"}}`, "", ""},
		{"enabled ok", `{"port":"50051","a2a":{"enabled":true,"port":8093}}`, "", ""},
		{"enabled no port", `{"port":"50051","a2a":{"enabled":true}}`, "", "server.a2a.port must be"},
		{"port is grpc port", `{"port":"50051","a2a":{"enabled":true,"port":50051}}`, "", "is server.port"},
		{"provider half set", `{"port":"50051","a2a":{"enabled":true,"port":8093,"provider_organization":"x"}}`, "", "set together"},
		{"bad binding", `{"port":"50051","a2a":{"enabled":true,"port":8093,"interfaces":[{"url_template":"https://x/{agent_id}","protocol_binding":"SOAP"}]}}`, "", "protocol_binding"},
		{"good binding", `{"port":"50051","a2a":{"enabled":true,"port":8093,"interfaces":[{"url_template":"https://x/{agent_id}","protocol_binding":"JSONRPC"}]}}`, "", ""},
		{"bad policy", `{"port":"50051"}`, `{"signature_policy":"maybe"}`, "a2a.signature_policy"},
		{"required without keys", `{"port":"50051"}`, `{"signature_policy":"required","endpoints":["https://r.example"]}`, "trusted_jwks_file"},
		{"required with keys", `{"port":"50051"}`, `{"signature_policy":"required","endpoints":["https://r.example"],"trusted_jwks_file":"k.json"}`, ""},
		{"endpoint not a url", `{"port":"50051"}`, `{"endpoints":["r.example"]}`, "must be an http(s) URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadConfig(writeA2AConfig(t, tc.server, tc.a2a))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}
