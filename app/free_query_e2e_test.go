package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2232 end to end through the real path (owner ruling 2026-11-19 on
// ENG-1400: the LLM never writes SQL, ADR-0149 D6). A database MCP server
// installed from the built-in catalog's `postgres` entry — with a tool list
// copied before the catalog flagged execute_sql, i.e. no free_query in its own
// config — is mapped by mcpServerFromConfig under the boot sandbox policy,
// connected by the real mcp.Connector over streamable HTTP, its tools
// registered by the MCP tool sink, and called through the ExecuteTool RPC (the
// agent-facing boundary) on an executor whose free-query control is read from
// the default config. Even under tools_unrestricted, the agent cannot run
// free SQL; the typed tool works; an operator's exact grant opens it; a
// profile glob does not.

type fakeDB struct {
	mu   sync.Mutex
	sqls []string
}

type sqlArgs struct {
	SQL string `json:"sql"`
}

func (db *fakeDB) server() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake-toolbox", Version: "1.0.0"}, nil)
	text := func(v string) *mcpsdk.CallToolResult {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: v}}}
	}
	mcpsdk.AddTool(s, &mcpsdk.Tool{Name: "execute_sql", Description: "run any SQL"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, a sqlArgs) (*mcpsdk.CallToolResult, any, error) {
			db.mu.Lock()
			db.sqls = append(db.sqls, a.SQL)
			db.mu.Unlock()
			return text(`[{"n":1}]`), nil, nil
		})
	mcpsdk.AddTool(s, &mcpsdk.Tool{Name: "list_tables", Description: "list tables"},
		func(context.Context, *mcpsdk.CallToolRequest, noArgs) (*mcpsdk.CallToolResult, any, error) {
			return text(`["invoices","customers"]`), nil, nil
		})
	return s
}

func (db *fakeDB) ran() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]string(nil), db.sqls...)
}

type approveAll struct{}

func (approveAll) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	return domain.ApprovalDecision{Approved: true, Decided: true, ApproverID: "operator", State: domain.ApprovalApproved}, nil
}

func TestFreeQueryE2E_AgentCannotRunFreeSQLOnACatalogDBServer(t *testing.T) {
	db := &fakeDB{}
	ts := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return db.server() }, nil))
	t.Cleanup(ts.Close)

	cfg := config.DefaultConfig()
	catalog, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxPolicyFromConfig(cfg.MCP.Sandbox, catalog, nil)
	// A legacy install of the catalog's postgres entry: its own policy says
	// nothing about free_query.
	server := config.MCPServerConfig{
		ID: "erp-db", Transport: "http", Endpoint: ts.URL, AllowPrivateNetwork: true,
		CatalogID: "postgres", CatalogVersion: "1.15.0",
		Tools: []config.MCPToolConfig{
			{Name: "execute_sql", Dangerous: true, Effects: []string{"read", "write"}},
			{Name: "list_tables", Effects: []string{"read"}},
		},
	}
	server.Auth.Type = "none"

	ctx, cancel := context.WithCancel(context.Background())
	conn := mcp.NewConnector(mcp.CredentialSources{})
	t.Cleanup(func() { cancel(); conn.Close() })
	reg := domain.NewInMemoryToolRegistry()
	sink := newMCPToolSink(reg, &domain.ToolIndexer{Store: &sinkFakeStore{}, Embedder: sinkFakeEmb{}})
	conn.AddServer(ctx, mcpServerFromConfig(server, sb), sink, time.Hour)
	waitUntil(t, "erp-db tools registered", func() bool {
		_, a := reg.Get("mcp:erp-db/execute_sql")
		_, b := reg.Get("mcp:erp-db/list_tables")
		return a && b
	})
	if st, _ := reg.Get("mcp:erp-db/execute_sql"); !st.FreeQuery || !st.Dangerous {
		t.Fatalf("the catalog flag must reach the registered tool: %+v", st)
	}
	if st, _ := reg.Get("mcp:erp-db/list_tables"); st.FreeQuery {
		t.Fatal("list_tables is typed")
	}

	grants, err := domain.NewLayeredGrantsStore(domain.NewInMemoryGrantsStore())
	if err != nil {
		t.Fatal(err)
	}
	build := func(unrestricted bool) *subnetwork.Server {
		exec, err := domain.NewToolExecutor(
			domain.ToolExecutorControls{Registry: reg, Grants: grants, Authz: domain.AllowAllAuthorizer{},
				Approval: approveAll{}, Unrestricted: unrestricted,
				FreeQueryOpen: !cfg.Execution.Tools.FreeQuery.RequireExplicitGrant},
			domain.ToolExecutor{MCPHandler: &mcp.Handler{Sessions: conn}, InlineThreshold: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		return &subnetwork.Server{ToolExecutor: exec}
	}
	call := func(srv *subnetwork.Server, agent, tool, args string) *pb.ExecuteToolResponse {
		t.Helper()
		actx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", agent))
		resp, err := srv.ExecuteTool(actx, &pb.ExecuteToolRequest{ToolName: tool, ArgsJson: args})
		if err != nil {
			t.Fatalf("ExecuteTool %s: %v", tool, err)
		}
		return resp
	}
	const dropAll = `{"sql":"DELETE FROM invoices"}`

	// 1. tools_unrestricted: the bypass does not reach free SQL.
	unrestricted := build(true)
	resp := call(unrestricted, "analyst", "mcp:erp-db/execute_sql", dropAll)
	if !resp.Denied || !strings.Contains(resp.DenyReason, "free-query tool") {
		t.Fatalf("agent free-SQL call: want denied by the free-query guard, got %+v", resp)
	}
	typed := call(unrestricted, "analyst", "mcp:erp-db/list_tables", `{}`)
	if typed.Denied || typed.Error != "" || !strings.Contains(typed.ResultJson, "invoices") {
		t.Fatalf("typed tool must work: %+v", typed)
	}

	// 2. A profile agent whose allowlist globs the whole server gets the typed
	// tool but not free SQL.
	newProfileGrantSync(grants.Profile(), reg.All, []profileAgent{
		{ID: "profiled", Allow: domain.ToolAllowlist{"mcp:erp-db/*"}},
	}, cfg.Execution.Tools.FreeQuery.RequireExplicitGrant).Sync(ctx, true)
	restricted := build(false)
	if r := call(restricted, "profiled", "mcp:erp-db/execute_sql", dropAll); !r.Denied {
		t.Fatalf("a profile glob must not grant free SQL: %+v", r)
	}
	if r := call(restricted, "profiled", "mcp:erp-db/list_tables", `{}`); r.Denied || r.Error != "" {
		t.Fatalf("profile glob grants the typed tool: %+v", r)
	}
	if got := db.ran(); len(got) != 0 {
		t.Fatalf("no SQL may have reached the database yet, got %v", got)
	}

	// 3. The operator grants execute_sql by its exact name: it runs (through
	// approval, as a dangerous tool).
	if err := grants.ReplaceGrants(ctx, "analyst", []domain.ToolGrant{{Tool: "mcp:erp-db/execute_sql", Policy: domain.ToolResourcePolicy{AllowAll: true}}}); err != nil {
		t.Fatal(err)
	}
	ok := call(unrestricted, "analyst", "mcp:erp-db/execute_sql", `{"sql":"SELECT 1"}`)
	if ok.Denied || ok.Error != "" {
		t.Fatalf("an explicitly granted free-SQL call must run: %+v", ok)
	}
	if got := db.ran(); len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("database saw %v", got)
	}
}
