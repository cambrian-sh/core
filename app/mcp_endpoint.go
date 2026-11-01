package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
)

// The inbound Cambrian MCP endpoint's listener (ADR-0126 D3).
//
// It is its OWN listener rather than a mount on the ADR-0028 ingestion HTTP
// server: that server used to be built as `&http.Server{Addr: ":port"}` —
// plaintext, every interface, no TLS decision, no bind address honoured — and
// hanging the product's public front door on that mux would silently have
// un-done SEC-03 for the one surface strangers are meant to reach. Both now
// bind through the same serveHTTP path as the operator plane (ADR-0130 D3)
// and inherit its refusal: no cert and no explicit opt-in on a routable
// address is a boot error, not a plaintext port.

// startMCPEndpoint serves the Published Tool Surface over MCP, returning the
// running server so shutdown can drain it. A disabled endpoint returns (nil,
// nil) — the absent case is the default and is not an error.
//
// It is called BEFORE Connector.ConnectAll: MCP *client* init is synchronous and
// takes 40-45 s, during which the kernel looks up. An external agent running
// `claude mcp add` against a booting kernel must reach a live endpoint, not a
// timeout.
func startMCPEndpoint(cfg *config.Config, surface domain.PublishedToolSurface,
	authorizer domain.Authorizer, identity domain.IdentityResolver) (*http.Server, error) {
	m := cfg.Server.MCP
	if !m.Enabled {
		return nil, nil
	}

	// SEC-03: the transport decision is made inside serveHTTP, BEFORE binding
	// and on the same rules as the gRPC plane, so a configuration that would
	// serve this endpoint in plaintext to the network fails at boot rather than
	// after the port is open. It is made here too, first, so a refusable
	// configuration is reported before the handler is built.
	if _, _, err := transportCredentials(cfg); err != nil {
		return nil, fmt.Errorf("mcp endpoint: %w", err)
	}

	handler, err := mcpserve.NewHandler(mcpserve.Options{
		Surface: surface,
		Clients: m.Clients,
		// Resolved PER REQUEST through the late-bound ADR-0101 seam: the store is
		// attached to the process from Run, and a token read once at boot would be
		// one a rotation could never reach.
		ResolveSecret:          resolveNamedSecret,
		MaxConcurrentPerClient: m.MaxConcurrentPerClient,
		Authorizer:             authorizer,
		// ADR-0126 D4: the identity hop. Whichever plugin claimed the seam (nil in
		// OSS, where the surface stays the identity) — the SAME resolver the chat
		// ingresses use, so a client bound here and a person bound on Telegram are
		// one binding registry rather than two vocabularies for one question.
		Identity: identity,
		// ADR-0127 D1: a client with a durable owner binding beside its token is
		// a worker machine; resolved through the same late-bound named-secret
		// seam as the token itself, so issuance, rotation and revocation reach
		// running requests without a restart.
		WorkerOwner: func(client string) (string, bool) {
			return resolveNamedSecret(mcpserve.WorkerOwnerSecretName(client))
		},
		// D12: the server-instructions text every client receives at initialize.
		Instructions: mcpserve.CoreInstructions,
		// The same constant as the operator handshake (see SetHandshake), so the
		// two identity surfaces cannot report different kernels.
		ServerVersion: KernelVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp endpoint: %w", err)
	}

	// A configured client with no issued credential can never authenticate. Said
	// out loud at boot, because the alternative is an endpoint that answers 401
	// to a correctly-configured client and looks, from the client's side, exactly
	// like a wrong token.
	var unissued []string
	for _, c := range m.Clients {
		if _, ok := resolveNamedSecret(mcpserve.ClientSecretName(c)); !ok {
			unissued = append(unissued, c)
		}
	}
	if len(unissued) > 0 {
		slog.Warn("ADR-0126: mcp clients have no stored credential and cannot authenticate",
			"clients", unissued, "fix", "store one under the named secret mcp:client:<name>")
	}

	// ADR-0130 D3: bound and served by the one listener policy — the same
	// bind address, transport decision, timeouts and body cap as every other
	// HTTP listener the kernel opens.
	srv, addr, err := serveHTTP(cfg, strconv.Itoa(m.Port), handler, "mcp endpoint")
	if err != nil {
		return nil, err
	}

	// The activation line: ADR-tagged so a live run can be grepped for proof the
	// endpoint is really up, rather than trusted. Tool count included because
	// "serving with zero tools" is a legal state and the one most likely to be
	// mistaken for a broken deployment.
	slog.Info("ADR-0126: mcp endpoint serving", "addr", addr, "tools", len(surface),
		"clients", len(m.Clients))
	return srv, nil
}
