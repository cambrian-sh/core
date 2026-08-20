package mcpserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// stubTool is a published tool whose handler records the context it was invoked
// with — the property ADR-0126 D4 turns on is that identity arrives on the
// context, never in the arguments.
type stubTool struct {
	result    domain.PublishedToolResult
	err       error
	gotArgs   json.RawMessage
	principal domain.PrincipalRef
	surface   domain.SurfaceRef
}

func (s *stubTool) Invoke(ctx context.Context, args json.RawMessage) (domain.PublishedToolResult, error) {
	s.gotArgs = args
	s.principal = domain.PrincipalFromContext(ctx)
	s.surface = domain.SurfaceFromContext(ctx)
	return s.result, s.err
}

// denyAll is a decision point that refuses every question, standing in for the
// premium PDP. Only Authorize is exercised here; the other three are the port's
// remaining methods and must exist for the type to satisfy it.
type denyAll struct{}

func (denyAll) Authorize(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	return domain.AccessDecision{
		Allowed:   false,
		Resource:  req.Resource,
		Principal: req.Principal,
		Surface:   req.Surface,
		Reason:    domain.ReasonEffectNotPermitted,
		Detail:    "test policy denies every effect",
	}
}

func (denyAll) Filter(context.Context, domain.PrincipalRef, domain.SurfaceRef, domain.ResourceKind, []domain.Taggable) ([]domain.Taggable, []domain.AccessDecision) {
	return nil, nil
}

func (denyAll) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: domain.ReasonNotAuthorized}
}

func (denyAll) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: domain.ReasonNotAuthorized}
}

// bearerRoundTripper injects the credential the way every target client does
// (`claude mcp add --transport http --header`).
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(clone)
}

// serve stands the endpoint up in-process and returns a connected SDK session.
func serve(t *testing.T, opts Options, token string) *mcpsdk.ClientSession {
	t.Helper()
	if opts.ResolveSecret == nil {
		opts.Clients = []string{"ci-bot"}
		opts.ResolveSecret = staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"})
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(t.Context(), &mcpsdk.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}},
		// Stateless mode answers GET with 405 by design (spec 2026-07-28), so the
		// standalone SSE stream is not established.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// Zero tools is a LEGAL state, not a broken deployment: the endpoint initializes
// and answers tools/list with an empty list. E4 adds the four read-only tools;
// until then a kernel that serves nothing must still say so in protocol.
func TestEndpoint_ZeroToolsStillInitializesAndLists(t *testing.T) {
	session := serve(t, Options{}, "token-aaaa")
	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list against an empty surface failed: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("tools = %d, want 0", len(res.Tools))
	}
}

// The whole path end to end: credential → principal → surface → effect
// authorization → handler → result mapping.
func TestEndpoint_CallRoutesThroughToTheHandler(t *testing.T) {
	stub := &stubTool{result: domain.PublishedToolResult{
		Text:       "two results",
		Structured: map[string]any{"hits": 2},
		ReceiptRef: "corr-42",
	}}
	session := serve(t, Options{Surface: domain.PublishedToolSurface{{
		Owner: "core",
		Tool: domain.PublishedTool{
			Name:        "search_memory",
			Title:       "Search memory",
			Description: "cheap retrieval",
			InputSchema: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			Effects:     []domain.ToolEffect{domain.EffectRead},
			ReadOnly:    true,
		},
		Handler: stub,
	}}}, "token-aaaa")

	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "search_memory" {
		t.Fatalf("tools/list = %+v", listed.Tools)
	}
	if listed.Tools[0].Annotations == nil || !listed.Tools[0].Annotations.ReadOnlyHint {
		t.Error("ReadOnly did not reach the readOnlyHint annotation")
	}

	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{
		Name:      "search_memory",
		Arguments: map[string]any{"query": "invoices"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("call reported an error: %+v", res.Content)
	}

	// Identity reached the handler on the CONTEXT.
	if stub.principal.ID != "mcp:ci-bot" || stub.surface.Kind != domain.SurfaceMCP {
		t.Errorf("handler saw principal=%v surface=%v", stub.principal, stub.surface)
	}
	// Arguments arrived raw, so a handler validates against its own schema.
	if !strings.Contains(string(stub.gotArgs), "invoices") {
		t.Errorf("handler args = %s", stub.gotArgs)
	}
	// Text → content, Structured → structuredContent, ReceiptRef → _meta.
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want one text block", res.Content)
	}
	if text, ok := res.Content[0].(*mcpsdk.TextContent); !ok || text.Text != "two results" {
		t.Errorf("content[0] = %+v, want the rendered text", res.Content[0])
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok || structured["hits"] == nil {
		t.Errorf("structuredContent = %#v, want the structured value", res.StructuredContent)
	}
	if got := res.Meta[ReceiptMetaKey]; got != "corr-42" {
		t.Errorf("_meta[%s] = %v, want the receipt handle", ReceiptMetaKey, got)
	}
}

// A policy denial is a TOOL error, not a transport error. An MCP protocol error
// is invisible to the calling model, so a denial delivered that way reads as a
// broken server rather than a boundary — and the "sees why" half of the week-6
// test would have nowhere to land.
func TestEndpoint_EffectDenialIsAToolErrorNotATransportError(t *testing.T) {
	stub := &stubTool{result: domain.PublishedToolResult{Text: "should never run"}}
	session := serve(t, Options{
		Authorizer: denyAll{},
		Surface: domain.PublishedToolSurface{{
			Owner: "core",
			Tool: domain.PublishedTool{
				Name:        "search_memory",
				InputSchema: []byte(`{"type":"object"}`),
				Effects:     []domain.ToolEffect{domain.EffectRead},
				ReadOnly:    true,
			},
			Handler: stub,
		}},
	}, "token-aaaa")

	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "search_memory"})
	if err != nil {
		t.Fatalf("a denial must not surface as a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("a denied call reported success")
	}
	if stub.gotArgs != nil || stub.principal.ID != "" {
		t.Fatal("the handler RAN despite the denial; authorization must precede invocation")
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok || !strings.Contains(text.Text, string(domain.ReasonEffectNotPermitted)) {
		t.Errorf("denial text = %+v, want the decision's reason so the caller can act on it", res.Content[0])
	}
}

// A handler failure is likewise reported inside the result, so the model can
// self-correct instead of seeing the server disappear.
func TestEndpoint_HandlerFailureIsAToolError(t *testing.T) {
	session := serve(t, Options{Surface: domain.PublishedToolSurface{{
		Owner: "core",
		Tool: domain.PublishedTool{
			Name:        "search_memory",
			InputSchema: []byte(`{"type":"object"}`),
			Effects:     []domain.ToolEffect{domain.EffectRead},
		},
		Handler: &stubTool{err: context.DeadlineExceeded},
	}}}, "token-aaaa")

	res, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "search_memory"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if !res.IsError {
		t.Fatal("a failing handler reported success")
	}
}

// No credential, no session — the protocol handshake itself never happens.
func TestEndpoint_ConnectWithoutACredentialFails(t *testing.T) {
	handler, err := NewHandler(Options{
		Clients:       []string{"ci-bot"},
		ResolveSecret: staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"}),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(t.Context(), &mcpsdk.StreamableClientTransport{
		Endpoint:             ts.URL,
		DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		_ = session.Close()
		t.Fatal("an unauthenticated client established a session")
	}
}

// A schema the SDK would panic on is a BOOT error naming the plugin. The
// composition root must not die of a plugin's mistake, and the mistake must be
// attributable.
func TestNewHandler_RefusesANonObjectInputSchema(t *testing.T) {
	for _, schema := range []string{`{"type":"string"}`, `[1,2,3]`, `not json`} {
		_, err := NewHandler(Options{
			Clients:       []string{"ci-bot"},
			ResolveSecret: staticSecrets(map[string]string{"mcp:client:ci-bot": "token-aaaa"}),
			Surface: domain.PublishedToolSurface{{
				Owner:   "some-plugin",
				Tool:    domain.PublishedTool{Name: "bad_tool", InputSchema: []byte(schema)},
				Handler: &stubTool{},
			}},
		})
		if err == nil {
			t.Fatalf("schema %q was accepted", schema)
		}
		if !strings.Contains(err.Error(), "some-plugin") || !strings.Contains(err.Error(), "bad_tool") {
			t.Errorf("schema %q: error names neither the plugin nor the tool: %v", schema, err)
		}
	}
}

// A tool declaring no arguments still needs a schema; the SDK requires the empty
// object rather than accepting absence, so the renderer supplies it.
func TestNewHandler_EmptySchemaBecomesTheEmptyObject(t *testing.T) {
	session := serve(t, Options{Surface: domain.PublishedToolSurface{{
		Owner:   "core",
		Tool:    domain.PublishedTool{Name: "list_documents"},
		Handler: &stubTool{result: domain.PublishedToolResult{Text: "ok"}},
	}}}, "token-aaaa")

	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) != 1 {
		t.Fatalf("tools = %+v", listed.Tools)
	}
	schema, _ := json.Marshal(listed.Tools[0].InputSchema)
	if !strings.Contains(string(schema), `"object"`) {
		t.Errorf("input schema = %s, want an object-typed default", schema)
	}
}
