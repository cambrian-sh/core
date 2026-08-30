// Package mcpserve renders the Published Tool Surface as the inbound Cambrian
// MCP endpoint (ADR-0126 D3/D4).
//
// The direction matters and is easy to get backwards. Its sibling
// internal/infrastructure/mcp is the CONNECTOR: Cambrian as a client, dialling
// foreign servers (ADR-0043). This package is the other way round — Cambrian as
// a server, answering external agents. Nothing published here ever comes from
// there: republishing a foreign server's tools would exercise Cambrian's
// credentials on behalf of arbitrary token holders, which is a confused deputy,
// and the parity ledger records it as a permanent exclusion.
//
// What is genuinely new here is only the RENDERING. The tools are
// domain.PublishedTool values composed by the plugin registry; the identity is
// domain.PrincipalRef; the policy question is domain.Authorizer. This package
// owns the SDK types and nothing else, which is what keeps a second transport
// (an HTTP/OpenAPI surface, an ACP adapter) additive rather than a rewrite.
package mcpserve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ReceiptMetaKey carries domain.PublishedToolResult.ReceiptRef on a tool
// result's `_meta`.
//
// Prefixed with the project's own domain per the MCP convention for `_meta`
// keys (and spec 2026-07-28's reverse-DNS extension ids), so a client merging
// metadata from several servers cannot collide with ours. It is metadata rather
// than a result field on purpose: the handle is for auditing the answer, not
// part of the answer, and a model should not have to read past it.
const ReceiptMetaKey = "cambrian.sh/receipt_ref"

const (
	// defaultServerName is the implementation name external clients see. Claude
	// Code renders tools as `mcp__<server>__<tool>`, so this is the namespace the
	// tool names deliberately do NOT carry themselves (ADR-0126 D7).
	defaultServerName = "cambrian"
	// defaultServerVersion is used when the composition root names none.
	defaultServerVersion = "0.0.0-dev"
	// surfaceID identifies the entry point on the `mcp` surface, beside
	// "console" for the operator plane and "grpc" for the agent plane. It names
	// the TRANSPORT, never the caller: the caller is the principal, and ADR-0126
	// D4 is explicit that the two facts do not substitute for each other.
	surfaceID = "endpoint"
	// principalPrefix namespaces an external client's principal id. A policy can
	// then target one client (`mcp:ci-bot`) as precisely as it targets an
	// internal agent.
	principalPrefix = "mcp:"
	// secretPrefix is the ADR-0101 named-secret prefix a client's bearer token is
	// stored under: `mcp:client:<name>`.
	secretPrefix = "mcp:client:"
	// workerOwnerPrefix is the ADR-0101 named entry a machine credential's owner
	// principal is stored under: `mcp:worker-owner:<name>` (ADR-0127 D1, the
	// `--owner` flag on token create). Beside the token on purpose: the owner
	// binding must live and die with the credential — durable across kernel
	// restarts, revoked with it — and a binding stored anywhere else could
	// survive the token it qualifies.
	workerOwnerPrefix = "mcp:worker-owner:"
)

// ClientSecretName is the ADR-0101 named secret holding one client's bearer
// token. Exported so the issuing side and the checking side cannot drift: a
// token written under a name the endpoint does not look up is a credential that
// exists and never works, which is the least diagnosable of failures.
func ClientSecretName(client string) string { return secretPrefix + client }

// WorkerOwnerSecretName is the ADR-0101 named entry holding one machine
// client's owner principal id (ADR-0127 D1). Exported for the same reason
// ClientSecretName is: the issuing side (`cambrian mcp token create --owner`)
// and the checking side (Options.WorkerOwner) must not drift.
func WorkerOwnerSecretName(client string) string { return workerOwnerPrefix + client }

// Options is everything the endpoint needs from the composition root.
type Options struct {
	// Surface is the composed Published Tool Surface. EMPTY IS LEGAL: a kernel
	// with no published tools still initializes and answers tools/list with an
	// empty list, which is a different (and honest) answer from failing to
	// serve.
	Surface domain.PublishedToolSurface
	// Clients are the configured client names (server.mcp.clients). Each resolves
	// to a stored credential; a name whose credential is absent can never
	// authenticate.
	Clients []string
	// ResolveSecret reads a named credential through the ADR-0101 store seam. It
	// is called PER REQUEST rather than at construction, because the store is
	// attached to the process late and a credential cached at boot would be one
	// a rotation could not reach.
	ResolveSecret func(name string) (value string, ok bool)
	// MaxConcurrentPerClient bounds in-flight requests per credential; ≤ 0 uses
	// config.DefaultMCPMaxConcurrentPerClient. There is no "off".
	MaxConcurrentPerClient int
	// Authorizer is the decision point. nil is not a failure — it means no policy
	// plugin is installed and domain.AuthorizerFromContext's allow-all fallback
	// governs, which is the correct semantics for an unscoped OSS deployment and
	// exactly why the bearer token is never optional.
	Authorizer domain.Authorizer
	// Identity answers "who is this external sender?" for the authenticated client
	// (ADR-0126 D4). Carried here for exactly the same reason Authorizer is: the
	// composition root holds whatever plugin claimed the seam, and this package
	// must be able to name the port without importing either side.
	//
	// nil is the OSS shape and is NOT a failure — no binding registry exists, so
	// the surface stays the identity and the client's own name is its principal.
	// Installed, it is what lets an operator re-point `mcp:<client>` at a
	// principal of their choosing, block one outright, or govern a client nobody
	// bound through the surface's stranger policy.
	Identity domain.IdentityResolver
	// WorkerOwner resolves a client name to the owner principal id its machine
	// credential was bound to at issuance (`cambrian mcp token create <machine>
	// --owner <owner-principal>`, ADR-0127 D1), read through the same ADR-0101
	// store the token lives in. A hit means this client IS a worker machine: it
	// authenticates as the machine principal (`machine:<name>`) carrying its
	// owner principal on the context, and the Identity hop is skipped — the
	// owner binding was made at issuance and is exactly the fact a binding
	// would otherwise state. nil, or a miss, means an ordinary client
	// (phase-1/2 behaviour verbatim).
	WorkerOwner func(client string) (owner string, ok bool)
	// ServerName / ServerVersion are the implementation identity reported at
	// initialize.
	ServerName    string
	ServerVersion string
	// Instructions is the D12 server-instructions text returned at initialize —
	// when to search vs ask, what the [n] markers mean. Tools that are listed
	// but never called are the observed failure mode of memory MCP servers;
	// this string is the counter-measure. Empty omits the field.
	Instructions string
}

// NewHandler builds the endpoint: an MCP server rendered from the surface,
// served over the SDK's streamable HTTP transport, behind the D4 middleware.
//
// The middleware is on the OUTSIDE of the SDK handler, not inside a tool
// handler, so that identity is established before any protocol handling runs —
// there is no ordering in which an unauthenticated request reaches a tool.
func NewHandler(opts Options) (http.Handler, error) {
	srv, err := newServer(opts)
	if err != nil {
		return nil, err
	}
	// Stateless per spec 2026-07-28 (ADR-0126 Context §"The protocol moved"):
	// nothing about a caller lives in transport state — the middleware
	// re-establishes it per request — so sessions would only add affinity a
	// hosted deployment then has to load-balance around. The SDK negotiates
	// down to 2025-11-25 for older clients.
	//
	// DNS-rebinding protection is left ON (DisableLocalhostProtection unset):
	// the common deployment is a loopback bind, which is precisely the case a
	// rebinding attack targets.
	sdk := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{Stateless: true},
	)
	return newAuthMiddleware(opts, sdk)
}

// newServer renders one mcpsdk.Server from the surface.
func newServer(opts Options) (*mcpsdk.Server, error) {
	name := opts.ServerName
	if name == "" {
		name = defaultServerName
	}
	version := opts.ServerVersion
	if version == "" {
		version = defaultServerVersion
	}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: version},
		&mcpsdk.ServerOptions{Instructions: opts.Instructions})
	effects := make(map[string][]domain.ToolEffect, len(opts.Surface))
	machineOnly := make(map[string]bool, len(opts.Surface))
	owners := make(map[string]string, len(opts.Surface))
	for _, entry := range opts.Surface {
		// Registry.PublishTool already rejects duplicates AMONG PLUGINS, but the
		// composed surface is plugin tools appended to the core ones — so a plugin
		// claiming a core name arrives here unchallenged, and Server.AddTool would
		// silently let the second registration win. Two answers to one question
		// with no way to say which held: a boot error naming both owners.
		if prev, dup := owners[entry.Tool.Name]; dup {
			return nil, fmt.Errorf("mcpserve: tool %q is published by both %q and %q",
				entry.Tool.Name, prev, entry.Owner)
		}
		owners[entry.Tool.Name] = entry.Owner
		tool, err := sdkTool(entry.Tool)
		if err != nil {
			return nil, fmt.Errorf("mcpserve: plugin %q: %w", entry.Owner, err)
		}
		// Server.AddTool, not the generic mcpsdk.AddTool: a published tool
		// carries a hand-authored JSON Schema rather than a Go input type, and
		// the generic form infers the schema from a type parameter we do not
		// have.
		srv.AddTool(tool, invoke(entry))
		effects[entry.Tool.Name] = entry.Tool.Effects
		machineOnly[entry.Tool.Name] = entry.Tool.MachineOnly
	}
	// ADR-0126 D8, listing side. The call side (invoke) is what actually holds —
	// a client may call a tool it was never shown — but a read-only principal
	// being SHOWN `remember` is an invitation to a refusal, and the filter is
	// what keeps the menu honest per caller.
	srv.AddReceivingMiddleware(listToolsFilter(effects, machineOnly))
	return srv, nil
}

// listToolsFilter drops tools whose declared effects this caller may not
// exercise from tools/list answers. Same decision point, same question as the
// call side — one policy, asked at two moments. A machine-only tool (ADR-0127
// D3) is additionally dropped for every non-machine principal: the worker
// transport is not part of any ordinary client's surface, and listing it
// would advertise a refusal.
func listToolsFilter(effects map[string][]domain.ToolEffect, machineOnly map[string]bool) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}
			listed, ok := res.(*mcpsdk.ListToolsResult)
			if !ok {
				return res, err
			}
			isMachine := domain.PrincipalFromContext(ctx).Kind == domain.PrincipalMachine
			kept := make([]*mcpsdk.Tool, 0, len(listed.Tools))
			for _, t := range listed.Tools {
				if machineOnly[t.Name] && !isMachine {
					continue
				}
				dec := authorizeCall(ctx, domain.PublishedTool{Name: t.Name, Effects: effects[t.Name]})
				if dec.Allowed {
					kept = append(kept, t)
				}
			}
			listed.Tools = kept
			return listed, nil
		}
	}
}

// sdkTool converts one declaration, refusing a schema the SDK would panic on.
//
// Server.AddTool panics on a non-object input schema. A plugin's mistake must
// be a boot error naming the plugin, not a panic in the composition root, so the
// same conditions are checked here first.
func sdkTool(t domain.PublishedTool) (*mcpsdk.Tool, error) {
	schema := json.RawMessage(t.InputSchema)
	if len(bytes.TrimSpace(schema)) == 0 {
		// A tool that takes no arguments still needs a schema; the SDK requires
		// the empty object rather than accepting absence.
		schema = json.RawMessage(`{"type":"object"}`)
	}
	var probe map[string]any
	if err := json.Unmarshal(schema, &probe); err != nil {
		return nil, fmt.Errorf("tool %q: input schema is not a JSON object: %w", t.Name, err)
	}
	if probe["type"] != "object" {
		return nil, fmt.Errorf(`tool %q: input schema must be object-typed at the top level (got type %v)`,
			t.Name, probe["type"])
	}
	return &mcpsdk.Tool{
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
		InputSchema: schema,
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: t.ReadOnly, Title: t.Title},
	}, nil
}

// invoke wraps one published handler as an SDK tool handler: authorize the
// declared effects, then run, then map the result.
func invoke(entry domain.PublishedToolEntry) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		// ADR-0127 D3: a machine-only tool refuses every non-machine principal
		// at the call side too — a client may call a tool it was never shown.
		if entry.Tool.MachineOnly && domain.PrincipalFromContext(ctx).Kind != domain.PrincipalMachine {
			return toolError("refused: this tool is callable only by worker machines (machine:* principals)"), nil
		}
		// ADR-0126 D8, call side. The listing filter is the other half; this one
		// is what actually holds, because a client may call a tool it was never
		// shown.
		if dec := authorizeCall(ctx, entry.Tool); !dec.Allowed {
			// A denial is a TOOL error, not a transport error: an MCP protocol
			// error is invisible to the model, so the caller would see a broken
			// server instead of a boundary it could reason about.
			return toolError("refused: " + dec.Explain()), nil
		}
		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		// ADR-0126 D6. One correlation handle per TOOL CALL, seeded before the
		// handler runs so everything the call touches — including the retrieval
		// path's per-hop decision records, which extend it as `<corr>-h1`,
		// `<corr>-h2` — is findable afterwards under one name.
		corr := newCorrelationID()
		ctx = domain.WithQueryCorrelation(ctx, corr)
		res, err := entry.Handler.Invoke(ctx, args)
		if err != nil {
			return toolError(err.Error()), nil
		}
		// A handler that produced its own handle keeps it (get_receipt answers
		// about a handle the caller supplied, not about itself). Everything else
		// gets the one this call seeded, so the answer and its audit trail are
		// joinable without the handler having to know a receipt lane exists.
		if res.ReceiptRef == "" {
			res.ReceiptRef = corr
		}
		return sdkResult(res), nil
	}
}

// newCorrelationID mints one call's handle: 128 bits of randomness, hex.
//
// Random rather than a counter or a timestamp because the handle is returned to an
// external caller — a sequence discloses how much traffic the deployment serves,
// and a timestamp discloses when. Neither is anything a caller needs to fetch its
// own receipt. On the (practically impossible) failure of the system entropy
// source the call still proceeds with an empty handle: no receipt reference is a
// degraded answer, a refused tool call is a broken one.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "mcp-" + hex.EncodeToString(b[:])
}

// authorizeCall puts the tool's declared effects to the decision point.
//
// The principal and surface come off the CONTEXT, established by the middleware
// — never from the arguments, which is the property domain.PublishedToolHandler
// documents and INV-5 requires.
func authorizeCall(ctx context.Context, t domain.PublishedTool) domain.AccessDecision {
	return domain.AuthorizerFromContext(ctx).Authorize(ctx, domain.AccessRequest{
		Principal: domain.PrincipalFromContext(ctx),
		Surface:   domain.SurfaceFromContext(ctx),
		Resource:  domain.ResourceRef{Kind: domain.KindTool, ID: t.Name},
		Effects:   t.Effects,
	})
}

// sdkResult maps a PublishedToolResult onto the wire shape.
func sdkResult(r domain.PublishedToolResult) *mcpsdk.CallToolResult {
	// Content is a required wire field with no omitempty, so an empty slice
	// rather than nil: a structured-only answer must not marshal `"content":null`
	// at clients that read the array unconditionally.
	out := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}
	if r.Text != "" {
		out.Content = []mcpsdk.Content{&mcpsdk.TextContent{Text: r.Text}}
	}
	out.StructuredContent = r.Structured
	if r.ReceiptRef != "" {
		out.Meta = mcpsdk.Meta{ReceiptMetaKey: r.ReceiptRef}
	}
	return out
}

// toolError renders a failure the calling model can see and act on.
func toolError(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
