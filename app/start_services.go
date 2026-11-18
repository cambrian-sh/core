package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/metabolism/backfill"
	"github.com/cambrian-sh/core/internal/ratelimit"
	"github.com/cambrian-sh/core/internal/substrate/operator"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// truncateRunes returns s bounded to at most n runes, appending an ellipsis marker
// when it had to cut. Rune-safe so a multi-byte prompt is never split mid-character.
// Used for the ADR-0079 exchange lane, where prompts can be many KB.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…[truncated]"
}

// llmExchangeEvent builds the ADR-0079 exchange event: prompt and completion
// truncated to maxChars RUNES, and PromptChars/ResponseChars the untruncated
// lengths in the SAME unit, so "count > maxChars" is exactly "this text was
// truncated". They were byte counts (len), which for non-ASCII text (TR/EN market)
// overstated the length and made untruncated text look truncated
// (T-20261113 ENG-1317).
func llmExchangeEvent(sessionID, agentID, modelID string, stepIndex int, prompt, completion string, maxChars int) domain.AgentLLMExchangeEvent {
	return domain.AgentLLMExchangeEvent{
		SessionID:     sessionID,
		AgentID:       agentID,
		StepIndex:     stepIndex,
		Purpose:       "agent_llm",
		ModelID:       modelID,
		Prompt:        truncateRunes(prompt, maxChars),
		Completion:    truncateRunes(completion, maxChars),
		PromptChars:   utf8.RuneCountInString(prompt),
		ResponseChars: utf8.RuneCountInString(completion),
	}
}

// startKernelServices starts the kernel's background workers and serves its
// planes, in this order (ENG-1123 split it into its four steps, unchanged):
// the stack workers and lifecycles, the plane authentication, the gRPC server,
// and the opt-in ingestion HTTP server.
func startKernelServices(g *errgroup.Group, ctx context.Context, k *Kernel) {
	startBackgroundWorkers(g, ctx, k)
	operatorIDP, agentAuth, planeLimiter, ok := buildPlaneAuth(g, ctx, k)
	if !ok {
		return // the boot already failed through g
	}
	serveGRPC(g, ctx, k, operatorIDP, agentAuth, planeLimiter)
	startIngestionHTTP(g, ctx, k, operatorIDP)
}

// startBackgroundWorkers starts the domain stacks' workers, the runtime
// diagnostics, the agent cards, the skill curator and ledger sweeper, the
// lifecycles (ADR-0074), the CircadianRhythm, the MCP watch loop and the
// profile backfill.
func startBackgroundWorkers(g *errgroup.Group, ctx context.Context, k *Kernel) {
	// A. Domain stack background workers
	g.Go(func() error { return k.Memory.Start(ctx) })
	g.Go(func() error { return k.Metabolism.Start(ctx) })
	g.Go(func() error { return k.Supervision.Start(ctx) })
	// Opt-in pprof listener and memory log line (runtime block; off by default).
	startRuntimeDiagnostics(g, ctx, k.Config)

	// ADR-0139 D4: the agent card listener (server.a2a) and remote A2A agent
	// registration (a2a.endpoints); both off by default. app/agentcard_endpoint.go.
	startAgentCards(g, ctx, k)

	// ADR-0139 D13: the deterministic skill curator (execution.skills.curator.enabled;
	// off by default). Non-blocking; drained by Shutdown.
	if k.SkillCurator != nil {
		k.SkillCurator.Start(ctx)
	}

	// ADR-0145 D1: the run ledger's retention sweeper (execution.ledger.enabled).
	if k.LedgerSweeper != nil {
		k.LedgerSweeper.Start(ctx)
	}

	// ADR-0074: start plugin lifecycles (e.g. the reactive engine's worker pools +
	// REACT-06 scheduler). Start is non-blocking (launches goroutines and returns) — the
	// reactive engine replays its REACT-01 journal and arms schedule watches here. Empty
	// in OSS (no plugins).
	for _, lc := range k.Lifecycles {
		if lc.Start != nil {
			lc.Start(ctx)
			slog.Info("ADR-0074: plugin lifecycle started", "name", lc.Name)
		}
	}

	// B. Synaptic Bridge background workers (ADR-0012)
	g.Go(func() error {
		return nil
	})
	g.Go(func() error {
		k.CircadianRhythm.Start(ctx)
		return nil
	})

	// ADR-0043 D8 / ADR-0044: MCP server health + reconnect loop. On a drop it
	// removes the server's tools from the menu + index; on reconnect it re-discovers
	// and re-indexes (re-sync). No-op when no MCP servers are configured.
	// Started even with ZERO configured servers (contract 0097): Watch blocks on
	// ctx and hosts the lifecycle that runtime-saved servers attach their
	// health loops under — gating it on the boot list would leave the first
	// console-added server of an MCP-less kernel unwatched.
	if k.MCPConnector != nil && k.MCPSink != nil {
		g.Go(func() error {
			k.MCPConnector.Watch(ctx, k.MCPServers, k.MCPSink, 30*time.Second)
			return nil
		})
	}

	// B. Async Backfill (Brain Integrity)
	g.Go(func() error {
		slog.Info("🧬 Kernel: Starting Brain Integrity verification (profile backfill)...")
		backfillCfg := backfill.BackfillConfig{TimeoutMs: 60000}
		if err := backfill.RunProfileBackfill(ctx, k.Registry, k.Memory.ProfileStore, k.Metabolism.ProfileSeeder, k.Memory.Embedder, backfillCfg); err != nil {
			return fmt.Errorf("brain integrity failure: %w", err)
		}
		slog.Info("✅ Kernel: Brain Integrity verified.")
		return nil
	})
}

// buildPlaneAuth resolves the operator identity, imports worker-machine owners,
// and builds the agent-plane credential verifier and the per-principal rate
// limiter both gRPC planes share. ok=false means the verifier could not be
// built: the failure is already on g and nothing may be served.
func buildPlaneAuth(g *errgroup.Group, ctx context.Context, k *Kernel) (domain.Authenticator,
	*authz.AgentCredentialVerifier, *ratelimit.Limiter, bool) {
	// ADR-0047: Operator Transport Plane auth (D13). The interceptors are
	// method-scoped to /cambrian.OperatorConsole/* (plus the Orchestrator
	// approval RPCs, ADR-0039 D10 — authz.IsOperatorGatedAgentServiceMethod).
	// ADR-0142 D1: the bearer is resolved through the kernel Authenticator (local
	// accounts seeded with the env bootstrap operator, plus one optional OIDC
	// issuer, or a plugin's replacement), built in bootstrapKernel.
	//
	// ONE identity for the process: the ingestion server's /v1/admin routes
	// (below) validate the same bearer through the same instance, so a token
	// minted by the gRPC Login is the token the HTTP admin surface accepts.
	operatorIDP := k.Authenticator
	// ADR-0142 migration step 6: worker-machine owners become registry persons.
	// Here, not in bootstrapKernel, because the owner bindings are named secrets
	// and the secret store is attached to the process by Run.
	importMachineOwners(ctx, k.Principals, k.Config.Server.MCP.Clients, resolveNamedSecret)

	// ADR-0130 D1/D2 + ADR-0157 D4: agent-plane client authentication. Every
	// Orchestrator RPC (and every premium service declared agent-facing)
	// requires a bearer with x-agent-id as the assertion it must match: a
	// kernel-spawned agent presents the per-instance token minted at its spawn
	// (the InstanceManager's registry — the same one SignalStream checks); an
	// external agent presents its stored named secret `agent:<id>:token`, which
	// a kernel-spawned agent may not use. server.insecure_localhost=true waives
	// the check for loopback callers (the benchmark harness runs under it).
	// Constructor-injected; nothing attaches it later (AGENTS.md §2.3).
	agentAuth, err := authz.NewAgentCredentialVerifier(resolveNamedSecret,
		k.Server.Manager.InstanceManager.Tokens(), kernelSpawnedAgent(k.Registry),
		k.Config.Server.InsecureLocalhost)
	if err != nil {
		// Fail the boot rather than serve an agent plane with no credential check.
		g.Go(func() error { return fmt.Errorf("ADR-0157: agent-plane verifier: %w", err) })
		return nil, nil, nil, false
	}
	switch {
	case agentAuth.Waived():
		slog.Warn("ADR-0130: agent-plane authentication DISABLED by server.insecure_localhost=true",
			"effect", "x-agent-id is trusted as the principal; any process reaching the port can act as any agent")
	case k.ConfigStore == nil:
		slog.Warn("ADR-0157: agent plane requires credentials; kernel-spawned agents use per-instance tokens, "+
			"but the config store is off so no EXTERNAL agent can authenticate", "fix",
			"enable the ADR-0101 store (CAMBRIAN_CONFIG_STORE) to issue agent:<id>:token for external agents, "+
				"or set server.insecure_localhost=true on a trusted host")
	default:
		slog.Info("ADR-0157: agent plane requires credentials",
			"kernel_spawned", "per-instance token (env CAMBRIAN_AGENT_TOKEN, minted at each spawn)",
			"external", "stored secret agent:<id>:token",
			"header", "authorization: Bearer + x-agent-id")
	}

	// Per-principal token bucket on both gRPC planes
	// (T-20261101-rate-limit-the-operator-and-agent-grpc-planes). Built here and
	// passed to the interceptor constructors below — never attached later
	// (AGENTS.md §2.3). Its interceptors run LAST in each chain, after agent and
	// operator authentication, so a bucket is keyed by the principal the
	// credential established; unauthenticated calls are keyed by peer host.
	planeLimiter := ratelimit.New(rateLimitConfig(k.Config.Server.RateLimit), nil)
	return operatorIDP, agentAuth, planeLimiter, true
}

// serveGRPC builds the gRPC server with the surface, credential and rate-limit
// interceptors, mounts every core and plugin service and the operator feed, and
// serves the main listener (and the kernel-only agents' unix sockets).
func serveGRPC(g *errgroup.Group, ctx context.Context, k *Kernel, operatorIDP domain.Authenticator,
	agentAuth *authz.AgentCredentialVerifier, planeLimiter *ratelimit.Limiter) {
	// C. gRPC Server
	g.Go(func() error {
		// SEC-03: recomputed from the same cfg rather than threaded through the
		// signature. transportCredentials is pure, so the two calls cannot disagree,
		// and Run has already failed the boot if this configuration is refusable.
		tlsOpts, _, terr := transportCredentials(k.Config)
		if terr != nil {
			// FAIL, never degrade. Run already refused this configuration before
			// binding, so reaching here means it changed underneath us — and the
			// tempting fallback (serve with no credentials) is precisely the
			// fail-open direction this file exists to close.
			return fmt.Errorf("SEC-03: refusing to start the gRPC server: %w", terr)
		}
		// ADR-0118 D3: the declared agent-plane premium services route auth —
		// exempt from the operator bearer, SurfaceAgent, x-agent-id principal.
		// Everything else under /cambrian.premium. now REQUIRES the bearer
		// (closing the pre-0118 pass-through).
		agentPlaneServices := make(map[string]bool, len(k.AgentGRPCServices))
		for name := range k.AgentGRPCServices {
			agentPlaneServices[name] = true
		}
		k.GRPC = grpc.NewServer(
			// TLS when configured. Empty when the plane is on loopback or an operator
			// explicitly opted into plaintext.
			append(tlsOpts,
				// ADR-0085 D7: stamp the surface FIRST, from the transport, so every
				// downstream handler knows where the request arrived from. It runs
				// unconditionally — the kernel always establishes the entry point; whether
				// that constrains anything is the decision point's business. Then the
				// credential interceptors: the agent plane's (ADR-0130 D1), which
				// establishes the agent principal, and the operator plane's (ADR-0047
				// D13), which gates the console and premium operator planes.
				grpc.ChainUnaryInterceptor(
					authz.UnarySurfaceInterceptor(agentPlaneServices),
					authz.UnaryAgentAuthInterceptor(agentAuth, agentPlaneServices),
					operator.UnaryAuthInterceptor(operatorIDP, agentPlaneServices, operator.MethodRoles(k.OperatorMethodRoles)),
					ratelimit.UnaryInterceptor(planeLimiter)),
				grpc.ChainStreamInterceptor(
					authz.StreamSurfaceInterceptor(agentPlaneServices),
					authz.StreamAgentAuthInterceptor(agentAuth, agentPlaneServices),
					operator.StreamAuthInterceptor(operatorIDP, agentPlaneServices, operator.MethodRoles(k.OperatorMethodRoles)),
					ratelimit.StreamInterceptor(planeLimiter)),
				// The operator binary-ingest lane (IngestMemory) carries raw file bytes;
				// gRPC's 4 MiB default rejects any PDF above it server-side before docling
				// ever runs, so the documented upload path is unusable for real documents.
				// Raise both directions to maxGRPCMessageBytes (512 MiB), which matches the
				// UI client's tonic limit (ui/src-tauri/src/transport.rs MAX_MESSAGE_BYTES).
				grpc.MaxRecvMsgSize(maxGRPCMessageBytes),
				grpc.MaxSendMsgSize(maxGRPCMessageBytes),
			)...)
		pb.RegisterOrchestratorServer(k.GRPC, k.Server)
		// PLAT-03 / ADR-0065: standard grpc.health.v1 on the main listener. Starts the
		// readiness probe loop (DB ping) and, if configured, the /healthz HTTP shim.
		if k.Health != nil {
			healthpb.RegisterHealthServer(k.GRPC, k.Health.Server())
			k.Health.Start(ctx, time.Duration(k.Config.Server.HealthCheckIntervalSeconds)*time.Second)
			if hp := k.Config.Server.HealthzPort; hp > 0 {
				// ADR-0130 D3: the shim is served under the one listener policy —
				// server.bind_address (loopback by default, NOT every interface as
				// the old ":port" server was), TLS when the kernel has it,
				// timeouts, body cap. A probe on another host needs the
				// deployment's bind_address, like every other listener.
				g.Go(func() error {
					srv, _, err := serveHTTP(k.Config, strconv.Itoa(hp), k.Health.HealthzHandler(), "healthz shim")
					if err != nil {
						// Fail the boot, as the old ListenAndServe error did.
						return fmt.Errorf("ADR-0065 healthz shim: %w", err)
					}
					<-ctx.Done()
					shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					return srv.Shutdown(shutCtx)
				})
			}
		}
		// The sequenced operator feed for the Cambrian UI. The spool decouples the
		// synchronous EventBus from network clients; SubscribeBridge fans the
		// existing domain events into it; the projection folds plan state.
		operatorFeed := operator.NewSpool(operator.SpoolConfig{})
		operator.SubscribeBridge(k.EventBus, operatorFeed)
		// Latch + heartbeat the agent roster so a feed consumer that connects AFTER boot
		// (e.g. the benchmark harness in --no-supervise) reliably receives the current
		// agent→capabilities roster instead of racing spool eviction of the one-time boot
		// AgentReadyOp events — the empty-capabilities routing-measurement bug.
		rosterLatch := operator.NewRosterLatch(operatorFeed)
		k.EventBus.Subscribe(domain.EventTypeAgentReady, rosterLatch.Observe)
		rosterLatch.Seed(ctx, k.Registry) // manifest baseline — present before the first AgentReadyEvent
		rosterLatch.Start(ctx, 0)
		operatorProjection := operator.NewProjection()
		operator.SubscribeProjection(k.EventBus, operatorProjection)
		// The live step-output stream reaches the feed's live-only lane as
		// TokenChunkEvent (ADR-0139 amendment 2026-11-06; formerly ADR-0047 0047-23's
		// managed-proxy TokenSink): gateway deltas from both model lanes and
		// agent-pushed EmitStepOutput chunks.
		if k.StepOutputFeed != nil {
			k.StepOutputFeed.attach(operatorFeed.EmitEphemeral)
		}
		// ADR-0079: fork the full prompt+completion of every managed-proxy agent turn
		// onto the feed's live-only exchange lane so a benchmark can review every agent
		// output and reconstruct the internal ReAct loop. Gated (default off) because
		// prompts are large/sensitive; the sink truncates to a bounded size and records
		// the untruncated lengths so truncation is visible downstream.
		if k.Config.Execution.LLM.CaptureLLMExchanges {
			const maxExchangeChars = 8192
			k.Server.LLMExchangeSink = func(sessionID, agentID, modelID string, stepIndex int, prompt, completion string) {
				operatorFeed.EmitEphemeral(llmExchangeEvent(sessionID, agentID, modelID, stepIndex,
					prompt, completion, maxExchangeChars))
			}
		}
		// ADR-0098 / contract 0079: progress onto the operator feed's ephemeral lane.
		if k.Progress != nil {
			k.Progress.setFeed(operatorFeed.EmitEphemeral)
		}
		// ENG-1305: the operator plane is composed and constructed in one place; a
		// missing required seam fails the boot here instead of at an operator's click.
		operatorSvc, opErr := composeOperator(ctx, k, operatorFeed, operatorProjection)
		if opErr != nil {
			return opErr // fails the errgroup: the kernel does not boot
		}
		// ADR-0141: the credential provider's (premium accounts) audit sink
		// and plugin-plane mutation contract — the same audit store + feed and
		// command_id dedupe as every operator write.
		if k.credentialSeams != nil {
			k.credentialSeams.bind(operatorSvc)
		}
		pb.RegisterOperatorConsoleServer(k.GRPC, operatorSvc)
		// ADR-0073: let a downstream (premium) binary mount ADDITIONAL gRPC services it
		// defines in its own proto (e.g. the reactive control plane the benchmark harness
		// drives). Registered after the core services and before Serve; inherits the
		// server-level operator auth interceptors. nil in OSS ⇒ no extra services.
		if k.ExtraServices != nil {
			k.ExtraServices(k.GRPC)
		}
		// ADR-0118 D3: agent-plane premium services, mounted on the same server;
		// the interceptors already treat their declared names as agent plane.
		for _, mount := range k.AgentGRPCServices {
			mount(k.GRPC)
		}
		slog.Info("🧬 Cambrian Substrate Active", "port", k.Config.Server.Port)
		// ADR-0157 S2: every service is registered; the kernel-only agents'
		// unix sockets (opened as their instances spawned) serve from here on.
		if k.agentSockets != nil {
			k.agentSockets.start(k.GRPC)
		}
		return k.GRPC.Serve(k.Listener)
	})
}

// startIngestionHTTP serves the ingestion HTTP server (ADR-0028) when
// execution.ingestion.ingestion_http_port is set.
func startIngestionHTTP(g *errgroup.Group, ctx context.Context, k *Kernel, operatorIDP domain.Authenticator) {
	// D. Ingestion HTTP server (ADR-0028) — opt-in via IngestionHTTPPort > 0.
	//
	// ADR-0130 D3: served by the one listener policy (serveHTTP): bound on
	// server.bind_address rather than ":port" (every interface), refused in
	// plaintext on a routable bind, TLS when the kernel has it, header/idle
	// timeouts, every body capped. /v1/ingest keeps its own X-Ingest-Token
	// (constant-time, ADR-0028).
	//
	// Contract 0099 removed the /v1/admin routes that have an OperatorConsole
	// equivalent, so each mutation has ONE door, and it is audited:
	//   /v1/admin/consolidate              -> OperatorConsole/TriggerConsolidation
	//   /v1/admin/agents/{id}/scope        -> OperatorConsole/SetScope
	//   /v1/admin/agents/{id}/tool-grants  -> OperatorConsole/SetToolGrant + SetToolPolicy
	// One route remains: /v1/admin/agents/{id}/write-tags (ADR-0035 C2,
	// PolicyAdmin.SetAgentWriteTags) has NO operator-plane RPC yet, and the access
	// policy is inert without a write ceiling (premium cmd/cambrian-apply and the
	// access_policy benchmark's out-of-band setup depend on it). It requires the
	// OPERATOR bearer, validated through the same OperatorIdentity the gRPC
	// interceptor uses, so a Logout or an expired token closes it too. Moving it
	// onto OperatorConsole is T-20261101-write-tags-admin-route-has-no-operatorconsole.
	if port := k.Config.Execution.Ingestion.IngestionHTTPPort; port > 0 {
		mux := http.NewServeMux()
		// ADR-0142 (K-3): /v1/ingest is never anonymous. With an ingest token the
		// pusher presents X-Ingest-Token; without one it presents the operator
		// bearer (resolved through the same Authenticator as the gRPC plane), where
		// an empty token used to switch authentication off entirely.
		if tok := k.Config.Execution.Ingestion.IngestToken; tok != "" {
			mux.Handle("/v1/ingest", memory.NewWebhookReceiver(tok, k.Memory.IngestionManager.Enqueue))
		} else {
			slog.Warn("ADR-0142: execution.ingestion.ingest_token is empty; /v1/ingest requires the operator bearer",
				"fix", "set execution.ingestion.ingest_token for token-based pushers")
			mux.Handle("/v1/ingest", requireOperatorBearer(operatorIDP,
				memory.NewPreauthenticatedWebhookReceiver(k.Memory.IngestionManager.Enqueue)))
		}
		mux.Handle("/v1/admin/agents/", requireOperatorBearer(operatorIDP, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Path: /v1/admin/agents/{id}/write-tags
			rest := strings.TrimPrefix(r.URL.Path, "/v1/admin/agents/")
			agentID, suffix, ok := strings.Cut(rest, "/")
			if !ok || agentID == "" || suffix != "write-tags" {
				http.Error(w, "expected /v1/admin/agents/{id}/write-tags (scope and tool grants moved to "+
					"OperatorConsole SetScope / SetToolGrant / SetToolPolicy in contract 0099)", http.StatusNotFound)
				return
			}
			// Write-tags are POLICY administration, so they exist only when a policy
			// plugin is installed. An OSS build answers 501 rather than pretending to
			// have stored a boundary it will never enforce.
			if k.PolicyAdmin == nil {
				http.Error(w, "access-policy plugin not installed; scope administration unavailable", http.StatusNotImplemented)
				return
			}
			// ADR-0035 C2: set the agent's DefaultWriteTags (write classification).
			// Body: {"default_write_tags":["company_wide","analytics"]}
			var body struct {
				DefaultWriteTags []string `json:"default_write_tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid write-tags body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := k.PolicyAdmin.SetAgentWriteTags(r.Context(), agentID, body.DefaultWriteTags); err != nil {
				http.Error(w, "write-tags rejected: "+err.Error(), http.StatusBadRequest)
				return
			}
			slog.Info("ADR-0035: agent DefaultWriteTags updated", "agent_id", agentID, "tags", body.DefaultWriteTags)
			w.WriteHeader(http.StatusOK)
		})))
		srv, addr, err := serveHTTP(k.Config, strconv.Itoa(port), mux, "ingestion http server")
		if err != nil {
			// FAIL the boot, never degrade: the only degradation available is the
			// ":port"-on-every-interface server this replaced.
			g.Go(func() error { return fmt.Errorf("ADR-0130: %w", err) })
			return
		}
		slog.Info("🌐 Ingestion HTTP server started", "addr", addr,
			"queue_size", k.Config.Execution.Ingestion.IngestionQueueSize,
			"workers", k.Config.Execution.Ingestion.IngestionWorkers,
			"admin_auth", "operator bearer (ADR-0130 D3)")
		g.Go(func() error {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		})
	}
}
