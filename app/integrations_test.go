package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// End to end, on the real store, the real connector and a real (in-process)
// MCP server that demands a bearer token: install from a catalog entry, the
// tools appear in the registry with their policy, disable/enable stops and
// restarts it, a rotated secret is used after the live bounce, test connection
// works, remove withdraws everything and clears the secrets. Secret values never
// appear in a read.

type authServer struct {
	mu    sync.Mutex
	auths []string
	url   string
}

func (a *authServer) lastAuth() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.auths) == 0 {
		return ""
	}
	return a.auths[len(a.auths)-1]
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	a := &authServer{}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "acme", Version: "1.0.0"}, nil)
	type in struct {
		Q string `json:"q"`
	}
	type out struct {
		A string `json:"a"`
	}
	for _, n := range []string{"lookup", "write_note"} {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: n, Description: n},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, i in) (*mcpsdk.CallToolResult, out, error) {
				return nil, out{A: "pong " + i.Q}, nil
			})
	}
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auths = append(a.auths, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	a.url = ts.URL
	return a
}

type integrationRig struct {
	m      *integrationManager
	reg    *domain.InMemoryToolRegistry
	src    configWriteSource
	dir    string
	mcpRaw string
}

func newIntegrationRig(t *testing.T, acmeURL string) *integrationRig {
	t.Helper()
	mcpRaw := `{"mcp": {"servers": [{"id": "filesrv", "transport": "http", "endpoint": "http://127.0.0.1:1/never", "enabled": false, "allow_private_network": true}]}}`
	src, dir := d8Source(t, map[string]string{"mcp.json": mcpRaw})
	catPath := filepath.Join(dir, "catalog.local.json")
	cat := `{"schema_version":1,"entries":[{"id":"acme","name":"Acme CRM","category":"crm","version":"1.0","trust_tier":"community",
	  "fail_closed":true,
	  "server":{"transport":"http","endpoint":"` + acmeURL + `","allow_private_network":true,"auth":{"type":"bearer"},
	            "tools":[{"name":"lookup","effects":["read","egress"]}]},
	  "fields":[{"name":"ACME_TOKEN","label":"Acme token","secret":true,"required":true,"kind":"token"}],
	  "probe_tool":{"name":"lookup","arguments":{"q":"ping"}}}]}`
	if err := os.WriteFile(catPath, []byte(cat), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := integrations.Load(catPath)
	if err != nil {
		t.Fatal(err)
	}
	mcp.SetSecretResolver(src.store)
	t.Cleanup(func() { mcp.SetSecretResolver(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	conn := mcp.NewConnector()
	t.Cleanup(func() { cancel(); conn.Close() })
	reg := domain.NewInMemoryToolRegistry()
	sink := newMCPToolSink(reg, &domain.ToolIndexer{Store: &sinkFakeStore{}, Embedder: sinkFakeEmb{}})
	rt := &mcpRuntime{}
	boot, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range boot.MCP.Servers {
		rt.upsertConfig(s, mcpSourceFile)
	}
	src.applyMCPServer = func(s config.MCPServerConfig, source string) bool {
		rt.upsertConfig(s, source)
		if !s.IsEnabled() {
			conn.StopServer(ctx, s.ID, sink)
			return true
		}
		conn.AddServer(ctx, mcpServerFromConfig(s, rt.sandbox), sink, time.Hour)
		return true
	}
	src.detachMCPServer = func(id string) bool { rt.remove(id); conn.RemoveServer(ctx, id, sink); return true }
	src.bounceMCPServer = func(id string) bool {
		sc, ok := rt.get(id)
		if !ok || !rt.enabled(id) {
			return ok
		}
		conn.AddServer(ctx, sc, sink, time.Hour)
		return true
	}
	return &integrationRig{
		m:   &integrationManager{writer: src, catalog: catalog, runtime: rt, connector: conn, testTimeout: 20 * time.Second},
		reg: reg, src: src, dir: dir, mcpRaw: mcpRaw,
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *integrationRig) find(id string) *pb.IntegrationOp {
	for _, op := range r.m.Integrations() {
		if op.GetId() == id {
			return op
		}
	}
	return nil
}

func TestIntegrations_EndToEndLifecycle(t *testing.T) {
	srv := newAuthServer(t)
	r := newIntegrationRig(t, srv.url)
	ctx := context.Background()

	// Catalog: built-ins plus the deployment entry.
	var sawAcme, sawBrave bool
	for _, e := range r.m.Catalog() {
		sawAcme = sawAcme || (e.GetId() == "acme" && e.GetSource() == "deployment" && e.GetProbeTool() == "lookup")
		sawBrave = sawBrave || e.GetId() == "brave-search"
	}
	if !sawAcme || !sawBrave {
		t.Fatalf("catalog incomplete: acme=%v brave=%v", sawAcme, sawBrave)
	}

	// Enabling without the required credential is refused, not crash-looped.
	out, _, _ := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{CatalogId: "acme", Enabled: true})
	if out.Effect != operator.EffectRejected || !strings.Contains(out.Error, "ACME_TOKEN") {
		t.Fatalf("want a missing-secret rejection, got %+v", out)
	}

	out, op, err := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{
		CatalogId: "acme", Enabled: true, Secrets: map[string]string{"ACME_TOKEN": "tok-first-1234"},
	})
	if err != nil || out.Effect != operator.EffectLive || op.GetSource() != "store" {
		t.Fatalf("install: %+v %+v %v", out, op, err)
	}
	waitUntil(t, "acme ready", func() bool { return r.find("acme").GetHealth().GetState() == mcp.HealthReady })
	if srv.lastAuth() != "Bearer tok-first-1234" {
		t.Fatalf("server saw auth %q", srv.lastAuth())
	}
	// The registry updated live, with the declared policy and fail-closed for the rest.
	if tl, ok := r.reg.Get("mcp:acme/lookup"); !ok || tl.Dangerous {
		t.Fatalf("lookup tool: %+v %v", tl, ok)
	}
	if tl, ok := r.reg.Get("mcp:acme/write_note"); !ok || !tl.Dangerous {
		t.Fatalf("an unlisted tool must fail closed: %+v %v", tl, ok)
	}

	// The read model: secret set from the store with a time, never the value.
	acme := r.find("acme")
	if len(acme.GetSecrets()) != 1 || !acme.GetSecrets()[0].GetSet() || acme.GetSecrets()[0].GetSource() != "store" ||
		acme.GetSecrets()[0].GetUpdatedAtUnixMs() == 0 || len(acme.GetMissingRequired()) != 0 {
		t.Fatalf("secret state: %+v", acme.GetSecrets())
	}
	all, _ := json.Marshal(r.m.Integrations())
	if strings.Contains(string(all), "tok-first-1234") {
		t.Fatal("SECRET LEAK: a read returned the secret value")
	}
	if fs := r.find("filesrv"); fs == nil || fs.GetSource() != "file" || fs.GetEnabled() {
		t.Fatalf("the mcp.json server must be listed, disabled, from the file: %+v", fs)
	}

	// Disable: process stopped, tools gone, health stopped. Enable: back.
	if out, _, _ := r.m.SetEnabled(ctx, "acme", false); out.Effect != operator.EffectLive {
		t.Fatalf("disable: %+v", out)
	}
	if _, ok := r.reg.Get("mcp:acme/lookup"); ok {
		t.Fatal("tools survived disable")
	}
	if st := r.find("acme").GetHealth().GetState(); st != mcp.HealthStopped {
		t.Fatalf("health after disable: %s", st)
	}
	if out, _, _ := r.m.SetEnabled(ctx, "acme", true); out.Effect != operator.EffectLive {
		t.Fatalf("enable: %+v", out)
	}
	waitUntil(t, "acme ready again", func() bool { _, ok := r.reg.Get("mcp:acme/lookup"); return ok })

	// Rotate: the live bounce makes the new credential the one presented.
	if out, _, _ := r.m.SetSecret(ctx, "acme", "ACME_TOKEN", "tok-second-5678"); out.Effect != operator.EffectLive {
		t.Fatalf("rotate: %+v", out)
	}
	waitUntil(t, "new token used", func() bool { return srv.lastAuth() == "Bearer tok-second-5678" })
	if out, _, _ := r.m.SetSecret(ctx, "acme", "NOT_A_FIELD", "x"); out.Effect != operator.EffectRejected {
		t.Fatalf("an undeclared secret must be refused: %+v", out)
	}

	// Test connection on the live session, with the read-only probe.
	waitUntil(t, "ready for test", func() bool { return r.find("acme").GetHealth().GetState() == mcp.HealthReady })
	res, err := r.m.Test(ctx, "acme", true)
	if err != nil || !res.GetOk() || !res.GetUsedLiveSession() || !res.GetProbeOk() || !strings.Contains(res.GetProbeOutput(), "pong ping") {
		t.Fatalf("test: %+v %v", res, err)
	}

	// Durable: the next boot sees the store entry over mcp.json; mcp.json untouched.
	boot, _, err := config.LoadConfigWithStore(r.src.configPath, r.src.store)
	if err != nil {
		t.Fatal(err)
	}
	if len(boot.MCP.Servers) != 2 {
		t.Fatalf("next boot: %+v", boot.MCP.Servers)
	}
	if got := readFile(t, filepath.Join(r.dir, "mcp.json")); got != r.mcpRaw {
		t.Fatal("mcp.json was rewritten")
	}

	// Remove: tools withdrawn, secrets cleared, gone from the next boot.
	if out, err := r.m.Remove(ctx, "acme", false); err != nil || out.Effect != operator.EffectLive {
		t.Fatalf("remove: %+v %v", out, err)
	}
	if _, ok := r.reg.Get("mcp:acme/lookup"); ok {
		t.Fatal("tools survived removal")
	}
	if r.src.store.Configured(mcp.TokenSecretName("acme")) {
		t.Fatal("secret survived removal")
	}
	if r.find("acme") != nil {
		t.Fatal("still listed after removal")
	}
}

func TestIntegrations_InstallValidation(t *testing.T) {
	srv := newAuthServer(t)
	r := newIntegrationRig(t, srv.url)
	ctx := context.Background()
	cases := map[string]*pb.InstallIntegrationOpRequest{
		"unknown catalog id":         {CatalogId: "nope"},
		"undeclared setting":         {CatalogId: "postgres", Id: "pg", Settings: map[string]string{"LD_PRELOAD": "/evil.so"}},
		"secret passed as a setting": {CatalogId: "postgres", Id: "pg", Settings: map[string]string{"POSTGRES_PASSWORD": "pw"}},
		"required setting missing":   {CatalogId: "postgres", Id: "pg"},
		"id clash with mcp.json":     {CatalogId: "acme", Id: "filesrv"},
		"bad id":                     {CatalogId: "acme", Id: "Bad Id"},
		"custom default that writes": {Id: "c1", Custom: &pb.IntegrationCustomSpecOp{Transport: "stdio", Endpoint: "x",
			HasDefaultPolicy: true, DefaultPolicy: &pb.IntegrationToolPolicyOp{Effects: []string{"read", "write"}}}},
		"custom credential literal": {Id: "c2", Custom: &pb.IntegrationCustomSpecOp{Transport: "stdio", Endpoint: "x"},
			Settings: map[string]string{"MY_API_KEY": "k"}},
	}
	for name, req := range cases {
		if out, _, _ := r.m.Install(ctx, req); out.Effect != operator.EffectRejected {
			t.Errorf("%s: accepted (%+v)", name, out)
		}
	}
	// A valid disabled install of a multi-instance entry: defaults filled, the
	// password a secret_env name, nothing started, missing_required reported.
	out, op, _ := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{CatalogId: "postgres", Id: "pg-sales",
		Settings: map[string]string{"POSTGRES_HOST": "db", "POSTGRES_DATABASE": "sales", "POSTGRES_USER": "ro"}})
	if out.Effect != operator.EffectLive || op.GetEnabled() || op.GetSettings()["POSTGRES_PORT"] != "5432" {
		t.Fatalf("disabled install: %+v %+v", out, op)
	}
	if strings.Join(op.GetMissingRequired(), ",") != "POSTGRES_PASSWORD" {
		t.Fatalf("missing_required: %v", op.GetMissingRequired())
	}
	if out, _, _ := r.m.SetEnabled(ctx, "pg-sales", true); out.Effect != operator.EffectRejected {
		t.Fatalf("enable without the password must be refused: %+v", out)
	}
	if out, _, _ := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{CatalogId: "postgres", Id: "pg-hr",
		Settings: map[string]string{"POSTGRES_HOST": "db", "POSTGRES_DATABASE": "hr", "POSTGRES_USER": "ro"}}); out.Effect == operator.EffectRejected {
		t.Fatalf("a multi-instance entry must install twice: %+v", out)
	}
}

// RevertIntegrationToFile (T-20261106-integrations-no-revert-to-file-for-an-mcp):
// a console override of an mcp.json server, and a console removal of one, are
// both undone by dropping the store entry; the file entry applies again, live,
// and mcp.json is never written.
func TestIntegrations_RevertToFile(t *testing.T) {
	r := newIntegrationRig(t, "http://127.0.0.1:1/unused")
	ctx := context.Background()
	key := config.MCPServerOverrideKey("filesrv")
	storeHas := func() bool {
		o, _ := r.src.store.Overrides()
		_, ok := o[key]
		return ok
	}

	// Nothing to revert yet: the integration already follows the file.
	out, _, err := r.m.RevertToFile(ctx, "filesrv")
	if err != nil || out.Effect != operator.EffectRejected || !strings.Contains(out.Error, "already uses") {
		t.Fatalf("revert with no override: %+v %v", out, err)
	}
	// No file entry at all.
	if out, _, _ := r.m.RevertToFile(ctx, "nope"); out.Effect != operator.EffectRejected || !strings.Contains(out.Error, "no config file") {
		t.Fatalf("revert of an id no file declares: %+v", out)
	}

	// Override (enable from the console) → revert → disabled file entry, stopped.
	if out, _, _ := r.m.SetEnabled(ctx, "filesrv", true); out.Effect != operator.EffectLive {
		t.Fatalf("enable: %+v", out)
	}
	if op := r.find("filesrv"); op.GetSource() != mcpSourceStore || !op.GetOverridesFile() || !op.GetEnabled() {
		t.Fatalf("after enable: %+v", op)
	}
	out, op, err := r.m.RevertToFile(ctx, "filesrv")
	if err != nil || out.Effect != operator.EffectLive || !out.Set {
		t.Fatalf("revert override: %+v %v", out, err)
	}
	if op.GetSource() != mcpSourceFile || op.GetOverridesFile() || op.GetEnabled() || storeHas() {
		t.Fatalf("after revert: %+v (store entry present=%v)", op, storeHas())
	}
	waitUntil(t, "the file entry's disabled state is applied live", func() bool {
		h, ok := r.m.connector.Health("filesrv")
		return ok && h.State == mcp.HealthStopped
	})

	// Removal (tombstone) → revert → listed again from the file.
	if out, _ := r.m.Remove(ctx, "filesrv", false); out.Effect != operator.EffectLive {
		t.Fatalf("remove: %+v", out)
	}
	if r.find("filesrv") != nil || !storeHas() {
		t.Fatal("a removed file server must be gone and tombstoned")
	}
	out, op, _ = r.m.RevertToFile(ctx, "filesrv")
	if out.Effect != operator.EffectLive || op.GetSource() != mcpSourceFile || storeHas() || r.find("filesrv") == nil {
		t.Fatalf("revert tombstone: %+v %+v", out, op)
	}

	raw, err := os.ReadFile(filepath.Join(r.dir, "mcp.json"))
	if err != nil || string(raw) != r.mcpRaw {
		t.Fatalf("mcp.json was modified: %q (%v)", raw, err)
	}
}
