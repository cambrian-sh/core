package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// The shipped catalog loads, and every entry obeys the owner directive: a
// declared policy (or an explicit fail_closed), credentials as secret fields,
// pinned versions.
func TestBuiltinCatalogLoadsAndIsValid(t *testing.T) {
	cat, err := Load("")
	if err != nil {
		t.Fatalf("built-in catalog: %v", err)
	}
	entries := cat.Entries()
	if len(entries) < 20 {
		t.Fatalf("expected the curated set (>=20 entries), got %d", len(entries))
	}
	for _, e := range entries {
		if e.Source != SourceBuiltin {
			t.Errorf("%s: source %q", e.ID, e.Source)
		}
		if len(e.Server.Tools) == 0 && e.Server.DefaultToolPolicy == nil && !e.FailClosed {
			t.Errorf("%s: no policy declared", e.ID)
		}
		for _, f := range e.Fields {
			if f.Kind == "" {
				t.Errorf("%s/%s: kind not defaulted", e.ID, f.Name)
			}
		}
	}
	for _, want := range []string{"brave-search", "google-workspace", "postgres", "filesystem", "time", "github"} {
		if _, ok := cat.Get(want); !ok {
			t.Errorf("catalog lacks %s", want)
		}
	}
	pg, _ := cat.Get("postgres")
	if f, ok := pg.FieldByName("POSTGRES_PASSWORD"); !ok || !f.Secret {
		t.Errorf("postgres password must be a secret field: %+v", f)
	}
	if len(cat.Categories()) < 5 {
		t.Errorf("categories: %v", cat.Categories())
	}
}

func validEntry() Entry {
	return Entry{
		ID: "demo", Name: "Demo", Category: "utilities", Version: "1.0.0", TrustTier: TrustOfficial,
		Server: config.MCPServerConfig{Transport: "stdio", Endpoint: "npx", Args: []string{"-y", "demo@1.0.0", "${ROOT}"},
			DefaultToolPolicy: &config.MCPToolConfig{Effects: []string{"read"}}},
		Fields: []Field{{Name: "ROOT", Label: "Root", Kind: KindEnv, Required: true}},
		Probe:  &ProbeTool{Name: "anything"},
	}
}

func TestValidateRejectsUnsafeEntries(t *testing.T) {
	if errs := Validate(validEntry()); len(errs) > 0 {
		t.Fatalf("valid entry rejected: %v", errs)
	}
	cases := map[string]func(*Entry){
		"no policy": func(e *Entry) { e.Server.DefaultToolPolicy = nil },
		"safe default that writes": func(e *Entry) {
			e.Server.DefaultToolPolicy = &config.MCPToolConfig{Effects: []string{"read", "write"}}
		},
		"credential as a plain setting": func(e *Entry) {
			e.Fields = append(e.Fields, Field{Name: "DEMO_API_KEY", Label: "Key", Kind: KindEnv})
		},
		"kernel variable": func(e *Entry) {
			e.Fields = append(e.Fields, Field{Name: "CAMBRIAN_X", Label: "X", Kind: KindEnv})
		},
		"placeholder to a secret": func(e *Entry) { e.Fields[0].Secret = true },
		"unknown effect": func(e *Entry) {
			e.Server.DefaultToolPolicy = &config.MCPToolConfig{Effects: []string{"teleport"}}
		},
		"probe tool that writes": func(e *Entry) {
			e.Server.DefaultToolPolicy = nil
			e.FailClosed = true
		},
		"token on stdio": func(e *Entry) {
			e.Fields = append(e.Fields, Field{Name: "TOK", Label: "t", Secret: true, Kind: KindToken})
		},
		"bad trust tier": func(e *Entry) { e.TrustTier = "trusted-by-me" },
		"bad id":         func(e *Entry) { e.ID = "Has Spaces" },
	}
	for name, mutate := range cases {
		e := validEntry()
		mutate(&e)
		if errs := Validate(e); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDeploymentCatalogReplacesAndExtends(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog.json")
	body := `{"schema_version":1,"entries":[
	 {"id":"time","name":"Our clock","category":"utilities","version":"9","trust_tier":"official",
	  "server":{"transport":"stdio","endpoint":"uvx","args":["mcp-server-time==9"],"default_tool_policy":{"effects":["read"]}}},
	 {"id":"crm","name":"Our CRM","category":"crm","version":"1","trust_tier":"community","fail_closed":true,
	  "server":{"transport":"http","endpoint":"https://crm.internal/mcp","auth":{"type":"bearer"}},
	  "fields":[{"name":"CRM_TOKEN","label":"Token","secret":true,"required":true,"kind":"token"}]}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	tm, _ := cat.Get("time")
	if tm.Name != "Our clock" || tm.Source != SourceDeployment {
		t.Errorf("time not replaced: %+v", tm)
	}
	if crm, ok := cat.Get("crm"); !ok || crm.Source != SourceDeployment {
		t.Errorf("crm not added")
	}
	// A broken deployment file fails the load loudly rather than half-loading.
	if err := os.WriteFile(p, []byte(`{"schema_version":1,"entries":[{"id":"x","name":"x","category":"c","version":"1","trust_tier":"official","server":{"transport":"stdio","endpoint":"x"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Errorf("want policy error, got %v", err)
	}
}

// ADR-0141 owner position 5: Google Workspace is usable per person. The read
// entry's grant never carries a send/modify scope (a grant is per server and
// person), and on the write entry sending and event changes need approval.
func TestGoogleWorkspacePerUserEntries(t *testing.T) {
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	read, ok := cat.Get("google-workspace-personal")
	if !ok {
		t.Fatal("catalog lacks google-workspace-personal")
	}
	write, ok := cat.Get("google-workspace-personal-write")
	if !ok {
		t.Fatal("catalog lacks google-workspace-personal-write")
	}
	for _, e := range []Entry{read, write} {
		uc := e.Server.UserCredential
		if uc == nil || uc.Scope != config.CredentialPerUser || uc.Provider != "google" || uc.Inject != "bearer" {
			t.Errorf("%s: want per_user google bearer, got %+v", e.ID, uc)
		}
	}
	for _, s := range read.Server.UserCredential.Scopes {
		if strings.HasSuffix(s, "openid") || strings.HasSuffix(s, "email") || strings.HasSuffix(s, ".readonly") {
			continue
		}
		t.Errorf("read entry requests non-read scope %q", s)
	}
	if d := read.Server.DefaultToolPolicy; d == nil || d.Dangerous || hasMutatingEffect(d.Effects) {
		t.Errorf("read entry default policy must be read-only: %+v", d)
	}
	if !write.FailClosed || write.Server.DefaultToolPolicy != nil {
		t.Error("write entry must be fail-closed")
	}
	for _, e := range []Entry{read, write} {
		for _, name := range []string{"send_gmail_message", "draft_gmail_message", "manage_event"} {
			found := false
			for _, tp := range e.Server.Tools {
				if tp.Name == name {
					found = true
					if !tp.Dangerous {
						t.Errorf("%s: %s must be dangerous", e.ID, name)
					}
				}
			}
			if !found {
				t.Errorf("%s: %s not listed", e.ID, name)
			}
		}
	}
	if shared, _ := cat.Get("google-workspace"); shared.Server.CredentialScope() != config.CredentialShared {
		t.Error("google-workspace is the shared (single-account) entry")
	}
}
