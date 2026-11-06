package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentcard"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/connector"
	"golang.org/x/sync/errgroup"
)

// A2A Agent Cards (ADR-0139 D4/C2; internal/agentcard).
//
// Two directions, configured apart as MCP's are:
//   - server.a2a (INBOUND): the card listener serving each registered agent's
//     kernel-signed card at /a2a/{id}/.well-known/agent-card.json, the index and
//     the signing key's JWKS. Its own listener on the one listener policy
//     (serveHTTP, ADR-0130 D3), not a mount on the MCP endpoint: that handler
//     authenticates every request, and A2A discovery is an unauthenticated GET.
//   - a2a (OUTBOUND): remote agents registered at boot from a2a.endpoints,
//     their cards checked by agentcard.RemoteFetcher under a2a.signature_policy.

// A2ASigningKeySecret is the ADR-0101 named secret holding the card-signing
// key when server.a2a.signing_key_file is unset.
const A2ASigningKeySecret = "a2a:card-signing-key"

// newRemoteCardFetcher builds the verifying card fetcher every remote-card read
// goes through: boot registration (StorageHandle.RegisterA2AAgent) and the
// profile seeder's description vector. A configured trust file that cannot be
// read is a boot error: a deployment that pinned keys and silently trusted
// none would register cards it meant to check.
func newRemoteCardFetcher(cfg *config.Config) (*agentcard.RemoteFetcher, error) {
	f := &agentcard.RemoteFetcher{
		Inner:  connector.New(),
		Policy: cfg.A2A.SignaturePolicy,
		Keys:   agentcard.KeySet{},
	}
	if path := strings.TrimSpace(cfg.A2A.TrustedJWKSFile); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("a2a.trusted_jwks_file: %w", err)
		}
		ks, err := agentcard.ParseJWKS(b)
		if err != nil {
			return nil, fmt.Errorf("a2a.trusted_jwks_file %s: %w", path, err)
		}
		f.Keys = ks
	}
	return f, nil
}

// a2aRegistrar is the storage seam boot registration needs. *kernel.StorageHandle
// satisfies it; the Kernel carries the handle as an opaque io.Closer.
type a2aRegistrar interface {
	RegisterA2AAgent(ctx context.Context, endpoint string) error
}

// startAgentCards starts the card listener (when server.a2a.enabled) and the
// boot registration of a2a.endpoints (when any). Both are no-ops by default.
func startAgentCards(g *errgroup.Group, ctx context.Context, k *Kernel) {
	cfg := k.Config
	if cfg == nil {
		return
	}
	if eps := cfg.A2A.Endpoints; len(eps) > 0 {
		reg, ok := k.Store.(a2aRegistrar)
		if !ok {
			slog.Error("ADR-0139 D4: remote A2A agents configured but storage cannot register them", "endpoints", len(eps))
		} else {
			g.Go(func() error {
				registerRemoteA2AAgents(ctx, reg, eps, cfg.A2A.SignaturePolicy)
				return nil
			})
		}
	}
	if !cfg.Server.A2A.Enabled {
		return
	}
	g.Go(func() error {
		srv, err := startAgentCardListener(cfg, k.Registry)
		if err != nil {
			// Enabled and unable to serve is a boot failure, as a refused
			// healthz or MCP listener is: the operator asked for it.
			return fmt.Errorf("ADR-0139 D4 agent card listener: %w", err)
		}
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	})
}

// registerRemoteA2AAgents registers each endpoint. A remote that is down or
// refused is logged and skipped: one stranger's outage is not a kernel failure.
func registerRemoteA2AAgents(ctx context.Context, reg a2aRegistrar, endpoints []string, policy string) {
	ok := 0
	for _, ep := range endpoints {
		ep = strings.TrimRight(strings.TrimSpace(ep), "/")
		if err := reg.RegisterA2AAgent(ctx, ep); err != nil {
			slog.Error("ADR-0139 D4: remote A2A agent NOT registered", "endpoint", ep, "err", err)
			continue
		}
		ok++
	}
	slog.Info("ADR-0139 D4: remote A2A agents registered", "registered", ok, "configured", len(endpoints),
		"signature_policy", policy, "trust_tier", "third-party")
}

func startAgentCardListener(cfg *config.Config, registry domain.AgentDeclarationSource) (*http.Server, error) {
	if registry == nil {
		return nil, errors.New("no agent registry")
	}
	a := cfg.Server.A2A
	signer, err := loadCardSigner(a)
	if err != nil {
		return nil, err
	}
	opt := agentcard.BuildOptions{
		Provider:         agentcard.Provider{Organization: a.ProviderOrganization, URL: a.ProviderURL},
		DocumentationURL: a.DocumentationURL,
	}
	for _, it := range a.Interfaces {
		opt.Interfaces = append(opt.Interfaces, agentcard.InterfaceTemplate{URLTemplate: it.URLTemplate, ProtocolBinding: it.ProtocolBinding})
	}
	pub := &agentcard.Publisher{
		Source:        registry,
		Signer:        signer,
		Options:       opt,
		IncludeSystem: a.IncludeSystemAgents,
		BaseURL:       a.PublicBaseURL,
	}
	srv, addr, err := serveHTTP(cfg, strconv.Itoa(a.Port), pub.Handler(), "a2a agent card listener")
	if err != nil {
		return nil, err
	}
	slog.Info("ADR-0139 D4: agent card listener serving", "addr", addr, "kid", signer.KeyID(),
		"interfaces", len(opt.Interfaces), "path", "/a2a/{agent_id}"+agentcard.WellKnownPath)
	return srv, nil
}

// loadCardSigner resolves the card-signing key: the configured key file, else
// the encrypted config store's named secret, minting either on first boot. The
// key never reaches a log line; only its kid (a public-key thumbprint) does.
func loadCardSigner(a config.A2AEndpointConfig) (*agentcard.Signer, error) {
	jku := ""
	if base := strings.TrimRight(strings.TrimSpace(a.PublicBaseURL), "/"); base != "" {
		jku = base + "/.well-known/jwks.json"
	}
	if path := strings.TrimSpace(a.SigningKeyFile); path != "" {
		key, created, err := agentcard.LoadOrCreateKeyFile(path)
		if err != nil {
			return nil, err
		}
		s, err := agentcard.NewSigner(key, jku)
		if err != nil {
			return nil, err
		}
		if created {
			slog.Info("ADR-0139 D4: minted the agent card signing key", "file", path, "kid", s.KeyID())
		}
		return s, nil
	}
	stored, ok := resolveNamedSecret(A2ASigningKeySecret)
	created := false
	if !ok {
		h, err := agentcard.GenerateKey()
		if err != nil {
			return nil, err
		}
		if err := storeNamedSecret(A2ASigningKeySecret, h); err != nil {
			// No store and no key file: an ephemeral key would sign cards
			// nobody can verify after the next restart, so refuse instead.
			return nil, fmt.Errorf("no card-signing key: set server.a2a.signing_key_file or enable the config store "+
				"(named secret %s): %w", A2ASigningKeySecret, err)
		}
		stored, created = h, true
	}
	key, err := agentcard.ParseKey(stored)
	if err != nil {
		return nil, fmt.Errorf("named secret %s: %w", A2ASigningKeySecret, err)
	}
	s, err := agentcard.NewSigner(key, jku)
	if err != nil {
		return nil, err
	}
	if created {
		slog.Info("ADR-0139 D4: minted the agent card signing key", "secret", A2ASigningKeySecret, "kid", s.KeyID())
	}
	return s, nil
}
