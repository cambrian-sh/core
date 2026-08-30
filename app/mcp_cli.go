package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
)

// The `cambrian mcp` subcommand group (ADR-0126 D9 + E5): client-token
// lifecycle for the inbound MCP endpoint, and the stdio bridge.
//
// Token issuance is OFFLINE in phase 1, deliberately: the ADR-0101 store is
// bbolt, single-process by construction, and a live-issuance RPC would be an
// operator-contract event (§8 item 3 — the owner's call, phase 3 territory).
// The lock is therefore a feature here: `token create` against a running
// kernel fails with a message that says to stop the kernel, rather than
// hanging or corrupting anything.

// RunMCP dispatches `mcp token create|revoke|list` and `mcp bridge`.
// It returns a process exit code, matching RunSetup/RunStatus/RunStop.
func RunMCP(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, mcpUsage)
		return 2
	}
	switch args[0] {
	case "token":
		return runMCPToken(args[1:])
	case "bridge":
		return runMCPBridge(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp subcommand %q\n%s", args[0], mcpUsage)
		return 2
	}
}

const mcpUsage = `usage:
  cambrian mcp token create <client-name> [--dir <kernel-dir>] [--rotate] [--owner <owner-principal>]
  cambrian mcp token revoke <client-name> [--dir <kernel-dir>]
  cambrian mcp token list   [--dir <kernel-dir>]
  cambrian mcp bridge [--endpoint <url>]   (token via CAMBRIAN_MCP_TOKEN)

Token commands are OFFLINE: stop the kernel first (the config store is
single-process). The bridge relays a local stdio MCP client to a running
kernel's HTTP endpoint.

--owner registers the credential as a WORKER MACHINE's (ADR-0127): the client
authenticates as machine:<client-name>, owned by the named owner principal,
and its contributed tools attach only to that principal's tasks. The binding
is durable beside the token and revoked with it.
`

// runMCPToken handles the credential lifecycle against the ADR-0101 store.
func runMCPToken(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, mcpUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("mcp token "+verb, flag.ContinueOnError)
	dir := fs.String("dir", ".", "kernel base directory (the one holding configs/)")
	rotate := fs.Bool("rotate", false, "replace an already-issued credential")
	// ADR-0127 D1: ownership is bound at token issuance, nowhere else — the
	// worker's machine principal and its owner principal are one durable fact.
	owner := fs.String("owner", "", "owner principal this machine credential belongs to (registers a worker)")
	// Two-phase parse, because the flag package stops at the first positional:
	// `token create ci-bot --rotate` must mean what it says, not silently drop
	// the flag.
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	name := ""
	if rest := fs.Args(); len(rest) > 0 {
		name = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
	}

	store, err := OpenConfigStore(*dir)
	if err != nil {
		// bbolt's open timeout is the usual way this fails: another process —
		// almost always the kernel — holds the lock.
		fmt.Fprintf(os.Stderr, "cannot open the config store: %v\n"+
			"If the kernel is running, stop it first (`cambrian stop`) — token issuance is offline in phase 1.\n", err)
		return 1
	}
	if store == nil {
		fmt.Fprintf(os.Stderr, "the config store is disabled (%s=off); tokens have nowhere durable to live\n", ConfigStoreEnv)
		return 1
	}
	defer store.Close()

	switch verb {
	case "create":
		if !validMCPClientName(name) {
			fmt.Fprintln(os.Stderr, "client name must match ^[a-z][a-z0-9-]{0,47}$ (e.g. ci-bot, afsin-laptop)")
			return 2
		}
		if *owner != "" && !validOwnerPrincipal(*owner) {
			fmt.Fprintln(os.Stderr, "--owner must be a principal id: non-empty, no whitespace, at most 128 chars")
			return 2
		}
		secretName := mcpserve.ClientSecretName(name)
		if store.Configured(secretName) && !*rotate {
			fmt.Fprintf(os.Stderr, "a credential for %q is already issued; use --rotate to replace it\n", name)
			return 1
		}
		token, err := newMCPToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate token: %v\n", err)
			return 1
		}
		if err := store.SetSecret(secretName, token); err != nil {
			fmt.Fprintf(os.Stderr, "store token: %v\n", err)
			return 1
		}
		// The owner binding lands beside the token, durably (ADR-0127 D1): it
		// survives kernel restarts exactly as the token does, and revoke clears
		// both. A rotate WITHOUT --owner keeps an existing binding — re-issuing
		// a machine's credential does not orphan its fleet membership; re-issue
		// WITH --owner re-binds it, because issuance is where ownership lives.
		if *owner != "" {
			if err := store.SetSecret(mcpserve.WorkerOwnerSecretName(name), *owner); err != nil {
				fmt.Fprintf(os.Stderr, "store owner binding: %v\n", err)
				return 1
			}
			fmt.Printf("worker %q registered, owned by principal %q\n", name, *owner)
		}
		// The one and only display. The store is write-only from here (LastFour
		// aside) — that is the ADR-0101 property, not an inconvenience.
		fmt.Printf(`token issued for %q — shown ONCE, store it now:

  %s

Wire it up:
  1. add %q to server.mcp.clients (and set server.mcp.enabled + server.mcp.port)
  2. start the kernel
  3. claude mcp add --transport http cambrian http://localhost:<port> --header "Authorization: Bearer <token>"

The token survives kernel restarts. Rotate with:  cambrian mcp token create %s --rotate
`, name, token, name, name)
		return 0

	case "revoke":
		if name == "" {
			fmt.Fprint(os.Stderr, mcpUsage)
			return 2
		}
		if err := store.ClearSecret(mcpserve.ClientSecretName(name)); err != nil {
			fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
			return 1
		}
		// Central revocation is the point of durable machine credentials
		// (ADR-0127): the owner binding dies with the token, so a revoked
		// worker can neither authenticate nor be mistaken for a fleet member.
		if err := store.ClearSecret(mcpserve.WorkerOwnerSecretName(name)); err != nil {
			fmt.Fprintf(os.Stderr, "revoke owner binding: %v\n", err)
			return 1
		}
		fmt.Printf("credential for %q revoked. Remove it from server.mcp.clients too — a configured client with no credential is warned about at every boot.\n", name)
		return 0

	case "list":
		// Names come from config (the store cannot enumerate secrets — that is
		// deliberate; see storage.BoltConfigStore); issuance state comes from the
		// store. The join is exactly what an operator needs: who is configured,
		// and which of them can actually authenticate.
		cfg, _, err := config.LoadConfigWithStore(filepath.Join(*dir, "configs", "config.json"), configStoreOrNil(store))
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			return 1
		}
		clients := cfg.Server.MCP.Clients
		if len(clients) == 0 {
			fmt.Println("no clients configured (server.mcp.clients is empty)")
			return 0
		}
		for _, c := range clients {
			state := "NOT ISSUED — run: cambrian mcp token create " + c
			if store.Configured(mcpserve.ClientSecretName(c)) {
				state = "issued (…" + store.LastFour(mcpserve.ClientSecretName(c)) + ")"
			}
			// A worker's owner binding is shown in full — it is an identity, not
			// a credential, and an operator deciding whose fleet a machine is in
			// needs the name, not four characters of it.
			if ownerID := store.Resolve(mcpserve.WorkerOwnerSecretName(c), ""); ownerID != "" {
				state += "  [worker, owner: " + ownerID + "]"
			}
			fmt.Printf("  %-24s %s\n", c, state)
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown token subcommand %q\n%s", verb, mcpUsage)
		return 2
	}
}

// newMCPToken returns a 256-bit URL-safe credential.
func newMCPToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "cmcp_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// validOwnerPrincipal bounds the --owner value loosely on purpose: owner
// principal ids are whatever the identity plane binds to (IdentityBinding.
// BoundToID), which the kernel does not constrain — only the shapes that can
// never be an id (empty, whitespace-bearing, absurdly long) are refused.
func validOwnerPrincipal(owner string) bool {
	if owner == "" || len(owner) > 128 {
		return false
	}
	for _, r := range owner {
		switch r {
		case ' ', '\t', '\n', '\r':
			return false
		}
	}
	return true
}

// validMCPClientName bounds client names the same way published tool names are
// bounded, plus '-' (host-like names such as afsin-laptop are the common case).
func validMCPClientName(name string) bool {
	if name == "" || len(name) > 48 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9', r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// runMCPBridge is the D9 stdio bridge: an SDK client against the kernel's HTTP
// endpoint, relayed to a stdio server. A DUMB PIPE — no tool logic, no schema
// rewriting, no caching; one implementation of every tool, in-kernel, or the
// two transports drift.
func runMCPBridge(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("mcp bridge", flag.ContinueOnError)
	endpoint := fs.String("endpoint", os.Getenv("CAMBRIAN_MCP_ENDPOINT"), "kernel MCP endpoint URL (or CAMBRIAN_MCP_ENDPOINT)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The token rides an env var, never argv: process listings are world-readable
	// and shell histories are durable.
	token := os.Getenv("CAMBRIAN_MCP_TOKEN")
	if *endpoint == "" || token == "" {
		fmt.Fprintln(os.Stderr, "bridge needs --endpoint (or CAMBRIAN_MCP_ENDPOINT) and CAMBRIAN_MCP_TOKEN")
		return 2
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian-mcp-bridge", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint:   *endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
		// The endpoint is stateless (spec 2026-07-28): GET answers 405, so the
		// standalone SSE stream must not be attempted.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: cannot reach the kernel endpoint: %v\n", err)
		return 1
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: tools/list: %v\n", err)
		return 1
	}

	// The relayed server mirrors the kernel's identity and instructions, so a
	// client cannot tell the transports apart — which is the point.
	var instructions string
	if init := session.InitializeResult(); init != nil {
		instructions = init.Instructions
	}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"},
		&mcpsdk.ServerOptions{Instructions: instructions})
	for _, t := range listed.Tools {
		tool := t
		srv.AddTool(&mcpsdk.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			Annotations: tool.Annotations,
		}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var args any
			if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
				args = json.RawMessage(req.Params.Arguments)
			}
			// The kernel's answer passes through UNTOUCHED, tool errors and
			// _meta (the receipt handle) included.
			return session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool.Name, Arguments: args})
		})
	}

	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		return 1
	}
	return 0
}

// bearerTransport injects the credential on every relayed request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(clone)
}
