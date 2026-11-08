package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 D8 for MCP: unlisted tools fail closed, operators can declare read-only
// tools (per tool or per server), and stdio servers get an allowlisted environment.

// envServerFlag turns this test binary into a stdio MCP server that reports its own
// environment. Passed as an ARGUMENT, not an env var, because the scrub under test
// would strip an env-var switch before the child could read it.
const envServerFlag = "--mcp-env-server"

func TestMain(m *testing.M) {
	for _, a := range os.Args[1:] {
		if a == envServerFlag {
			runEnvServer()
			os.Exit(0)
		}
	}
	// goleak (AGENTS.md §3.3): the connector's per-server watch loops and the
	// stdio process plumbing must all be gone once a test has closed them.
	// Ignored: the SERVER side of the in-process Streamable-HTTP fixtures
	// (httptest servers built by these tests); closing the httptest listener does
	// not end the SDK's server sessions. That is fixture plumbing, not the
	// connector — a leaked watch loop, client session or stdio pipe still fails.
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}

type envOut struct {
	Env  []string `json:"env"`
	Args []string `json:"args"`
}

func runEnvServer() {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "envsrv", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "env", Description: "report the environment"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, envOut, error) {
			return nil, envOut{Env: os.Environ(), Args: os.Args[1:]}, nil
		})
	_ = server.Run(context.Background(), &mcpsdk.StdioTransport{})
}

// httpServerWith stands up a Streamable-HTTP MCP server advertising the named tools.
func httpServerWith(t *testing.T, names ...string) string {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "srv", Version: "1.0.0"}, nil)
	for _, n := range names {
		mcpsdk.AddTool(server, &mcpsdk.Tool{Name: n, Description: n},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
				return nil, echoOut{Echo: in.Value}, nil
			})
	}
	ts := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func toolByName(t *testing.T, tools []domain.SystemTool, name string) domain.SystemTool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s not discovered (got %d tools)", name, len(tools))
	return domain.SystemTool{}
}

func effectSet(es []domain.ToolEffect) map[domain.ToolEffect]bool {
	m := map[domain.ToolEffect]bool{}
	for _, e := range es {
		m[e] = true
	}
	return m
}

// An unlisted tool on a server with no default policy is Dangerous with
// write+egress — and stays so after the registry validates it.
func TestDiscover_UnlistedToolFailsClosed(t *testing.T) {
	t.Parallel()
	url := httpServerWith(t, "run_command")
	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(context.Background(), []ServerConfig{{ID: "shell", Transport: "http", Endpoint: url}})

	reg := domain.NewInMemoryToolRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	got, ok := reg.Get("mcp:shell/run_command")
	if !ok {
		t.Fatal("unlisted tool must still register (fail-closed, not absent)")
	}
	if !got.Dangerous {
		t.Error("an unlisted MCP tool must be Dangerous")
	}
	es := effectSet(got.Effects)
	if !es[domain.EffectWrite] || !es[domain.EffectEgress] {
		t.Errorf("an unlisted MCP tool must carry write+egress, got %v", got.Effects)
	}
	if got.EffectsInferred {
		t.Error("fail-closed effects are declared by policy, not inferred")
	}
}

// Explicit read-only policy works per tool and per server, and a per-tool entry wins
// over the server default.
func TestDiscover_ExplicitReadOnlyPolicy(t *testing.T) {
	t.Parallel()
	url := httpServerWith(t, "think", "write_note")
	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(context.Background(), []ServerConfig{{
		ID: "seq", Transport: "http", Endpoint: url,
		DefaultPolicy: &ToolPolicy{Effects: []string{"read"}},
		Tools:         map[string]ToolPolicy{"write_note": {Dangerous: true, Effects: []string{"read", "write"}}},
	}})

	think := toolByName(t, tools, "mcp:seq/think")
	if think.Dangerous {
		t.Error("server default read-only policy must make think non-dangerous")
	}
	if es := effectSet(think.Effects); len(es) != 1 || !es[domain.EffectRead] {
		t.Errorf("think effects = %v, want [read]", think.Effects)
	}
	note := toolByName(t, tools, "mcp:seq/write_note")
	if !note.Dangerous {
		t.Error("per-tool policy must win over the server default")
	}
	if es := effectSet(note.Effects); !es[domain.EffectWrite] || es[domain.EffectEgress] {
		t.Errorf("write_note effects = %v, want [read write]", note.Effects)
	}
}

// A per-tool entry without effects keeps the pre-D8 meaning: dangerous as stated,
// effects inferred by the registry.
func TestDiscover_ListedToolWithoutEffectsIsInferred(t *testing.T) {
	t.Parallel()
	url := httpServerWith(t, "read_file")
	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(context.Background(), []ServerConfig{{
		ID: "fs", Transport: "http", Endpoint: url,
		Tools: map[string]ToolPolicy{"read_file": {Dangerous: false}},
	}})
	reg := domain.NewInMemoryToolRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	got, _ := reg.Get("mcp:fs/read_file")
	if got.Dangerous || !got.EffectsInferred || len(got.Effects) != 1 || got.Effects[0] != domain.EffectRead {
		t.Errorf("listed tool: dangerous=%v inferred=%v effects=%v", got.Dangerous, got.EffectsInferred, got.Effects)
	}
}

// A stdio server sees no provider key or kernel secret unless its config names it.
// Not parallel: t.Setenv.
func TestStdio_EnvironmentIsAllowlisted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-leak")
	t.Setenv("OPENAI_API_KEY", "sk-oai-leak")
	t.Setenv("SOME_OTHER_API_KEY", "other-leak")
	t.Setenv("CAMBRIAN_SECRET_KEY", "master-leak")
	t.Setenv("UNRELATED_SETTING", "nope")
	t.Setenv("FIRECRAWL_API_KEY", "fc-passed")
	t.Setenv("TAU2_HUB_URL", "http://hub")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(ctx, []ServerConfig{{
		ID: "envsrv", Transport: "stdio", Endpoint: exe, Args: []string{envServerFlag},
		Env:           []string{"FIRECRAWL_API_KEY", "TAU2_HUB_URL", "CAMBRIAN_SECRET_KEY"},
		DefaultPolicy: &ToolPolicy{Effects: []string{"read"}},
		Sandbox:       testSandboxSpec(t, "envsrv"),
	}})
	toolByName(t, tools, "mcp:envsrv/env")

	h := &Handler{Sessions: conn}
	out, err := h.Execute(ctx, domain.ToolCall{ToolName: "mcp:envsrv/env", ArgsJSON: []byte(`{}`)})
	if err != nil {
		t.Fatalf("env call: %v", err)
	}
	child := string(out)
	for _, leaked := range []string{"sk-ant-leak", "sk-oai-leak", "other-leak", "master-leak", "nope"} {
		if strings.Contains(child, leaked) {
			t.Errorf("SECRET LEAK: the stdio server saw %q", leaked)
		}
	}
	for _, passed := range []string{"FIRECRAWL_API_KEY=fc-passed", "TAU2_HUB_URL=http://hub"} {
		if !strings.Contains(child, passed) {
			t.Errorf("configured passthrough %q did not reach the server", passed)
		}
	}
	if !strings.Contains(strings.ToUpper(child), "PATH=") {
		t.Error("the OS essentials (PATH) must still reach the server")
	}
}
