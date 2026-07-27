package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/cambrian-sh/core/domain"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// Options carries the injection hooks the composition root (Run / bootstrapKernel)
// uses to wire optional or proprietary components. OSS defaults are inert (no
// tracing, no agent-call logging, no reactive engine); a downstream (premium)
// binary supplies real implementations and reuses the same bootstrap. ADR-0057 (Model C).
//
// This is the OSS-exported extension surface. Keep it minimal — additions here are
// public API.
type Options struct {
	// TraceWrapper wraps every acquired generator at the Provider's Acquire
	// chokepoint (ADR-0042). OSS default: identity. Premium: a Langfuse wrapper.
	// A nil value disables wrapping.
	TraceWrapper func(g domain.Generator, subsystem string) domain.Generator

	// AgentCallLogger records agent-initiated LLM calls (GenerateViaModelStream).
	// OSS default: nil (the call site nil-checks it). Premium: a Langfuse logger.
	AgentCallLogger subnetwork.AgentCallLogger

	// NewSignalReceiver, when non-nil, builds the reactive SignalReceiver (+ watch
	// CRUD handler) from the OSS capability bundle. OSS default: nil → the Watcher is
	// used (LTM enrichment + Planner dispatch). The premium binary injects a function
	// that constructs the ReactiveEngine. ADR-0032 / ADR-0057.
	NewSignalReceiver func(KernelServices) (domain.SignalReceiver, domain.WatchConfigHandler)

	// ResourceSelector, when non-nil, replaces the config-driven (auction/EFE) routing
	// selector (ADR-0037) with a caller/plugin-supplied one — the Tier-1 replace-one
	// extension point for the selection mechanism (ADR-0074). OSS default: nil (config
	// decides). A plugin sets this via Registry.SetResourceSelector.
	ResourceSelector domain.ResourceSelector

	// Entitlements decides which plugins may activate (ADR-0082 D3). It is consulted at
	// the single chokepoint in applyPlugins, BEFORE a plugin registers, so an unentitled
	// plugin contributes nothing at all. nil ⇒ every declared plugin activates: the OSS
	// default, since the OSS build ships no paid plugins and so gates nothing. A premium
	// binary supplies a licence-backed provider once billing launches.
	//
	// Tier-3 never-pluggable: a plugin must NOT be able to set this (self-granting).
	Entitlements EntitlementProvider

	// Plugins is the compile-time plugin set (ADR-0074). Each plugin's Register declares
	// its contributions (signal receiver, extra gRPC services, trace wrapper, lifecycle
	// hooks…) which are folded into the effective Options at boot. Plugins coexist with
	// the directly-set fields above. OSS default: empty (no plugins).
	Plugins []Plugin

	// Authorizer is the access-control DECISION POINT (ADR-0085). OSS default: nil,
	// which the composition root replaces with domain.AllowAllAuthorizer — the
	// correct semantics for a single-tenant, unscoped open-source deployment. A
	// premium policy plugin supplies a real one, which fails CLOSED.
	//
	// Tier-1 replace-one (ADR-0074): at most one plugin may own the decision. The
	// ENFORCEMENT POINTS that consult it are not pluggable at all — a missing
	// plugin must mean "no policy", never "no check".
	Authorizer domain.Authorizer

	// IngressResolver answers which principals are registered ingresses and what
	// surface the kernel stamps on sessions they open (ADR-0090 D2/D3). nil — the
	// OSS default — means nothing is an ingress and every surface stays
	// transport-derived, which is exactly the behaviour before ADR-0090.
	//
	// It is separate from Authorizer on purpose: the Authorizer DECIDES using a
	// surface, this SUPPLIES one. Folding them would let a decision point choose
	// its own inputs.
	IngressResolver domain.IngressResolver

	// PolicyAdmin is the policy ADMINISTRATION surface behind the operator plane's
	// scope/vocabulary/explain RPCs. nil in OSS ⇒ those RPCs return Unimplemented,
	// the same shape as WatchConfigHandler.
	PolicyAdmin domain.PolicyAdmin

	// ExtraServices, when non-nil, is invoked with the kernel's gRPC server AFTER the
	// core services (Orchestrator, Health, OperatorConsole) are registered and BEFORE
	// Serve, letting a downstream (premium) binary mount ADDITIONAL gRPC services that
	// it defines in its OWN proto — so the OSS operator contract stays untouched
	// (ADR-0073). Any service mounted here inherits the server-level operator auth
	// interceptors, so it is authenticated exactly like the OperatorConsole plane, not
	// a bypass. OSS default: nil (no extra services). ADR-0057 (Model C) / ADR-0073.
	ExtraServices func(*grpc.Server)
}

// KernelServices is the OSS-provided capability bundle handed to every plugin's Build phase
// (ADR-0082 D7/D12). Every field is an interface — a plugin depends on these, never on the
// kernel stacks. This is the spike-validated seam (ADR-0057 D14).
//
// It is deliberately GENERIC: nothing here is reactive-specific except two leftovers.
// WatchStore and Journal still carry reactive vocabulary because replacing them with the
// namespaced PluginStore (ADR-0082 D7) requires domain.WatchConfig to leave the OSS domain
// package — which is blocked until the watch RPCs move to a premium-owned proto (D8).
// Until then they remain, documented as debt rather than silently tolerated.
type KernelServices struct {
	Manager    ReactiveAgentDispatcher // direct dispatch + daemon lifecycle
	Auctioneer domain.Auctioneer       // full Gatekeeper → Auction
	Memory     ReactiveMemoryWriter    // async LTM ingest
	Planner    ReactivePlanner         // plan generation for start_plan actions
	LLM        domain.Generator        // LLM condition evaluation
	WatchStore ReactiveWatchStore      // WatchConfig persistence (BBolt)
	EventBus   domain.EventBus         // daemon-crash subscription, emit_event
	// Journal is the durable-execution surface (REACT-01 / ADR-0061): signal
	// journal + per-watch ack cursor + exactly-once idempotency + dead-letter.
	// May be nil — a nil journal leaves the engine in its pure in-memory mode
	// (today's behavior), so OSS builds and existing tests are unaffected.
	Journal ReactiveJournal
	// AcquireLLMToken provisions a managed-LLM session token (ADR-0018) for a
	// direct-dispatch consumer that dispatches an agent OUTSIDE the planner/DAG path —
	// where tokens are normally issued (server.go:493). The ADR-0080 chat manager needs
	// this: without a `_session_token_id` on the handoff Context, the dispatched agent's
	// GenerateViaModelStream call is rejected UNAUTHENTICATED. Returns the token id and a
	// release func to call when the turn completes. Nil when no gateway is configured.
	AcquireLLMToken func(ctx context.Context, tokenLimit int, ttl time.Duration) (tokenID string, release func(), err error)

	// SQL is the kernel's Postgres pool, handed to plugins that own their own
	// tables (the policy plugin's agent_scopes / policy objects). nil when no
	// Postgres is configured — a plugin must degrade rather than panic.
	//
	// It is deliberately the concrete pool rather than an interface: a plugin that
	// owns tables owns their schema and their queries, and pretending otherwise
	// would mean re-exporting half of pgx through a seam nobody benefits from.
	SQL *pgxpool.Pool

	// AgentExists reports whether an agent is registered. The policy plugin needs
	// it to tell "registered but unprofiled" (unrestricted) apart from "unknown
	// principal" (fail-closed) — a distinction that decides whether a query
	// returns everything or nothing.
	AgentExists func(agentID string) bool

	// SessionScopes reads the non-forgeable per-session caller term from the
	// persisted session record. The policy plugin composes it with the rest; the
	// kernel never interprets it. nil when no session store is available.
	SessionScopes SessionScopeReader
}

// SessionScopeReader returns the caller term persisted on a session record. It is
// re-derived SERVER-SIDE and never read from a handoff payload — a caller that
// could name its own scope would not be a boundary at all (INV-5).
type SessionScopeReader interface {
	CallerScope(ctx context.Context, sessionID domain.SessionID) domain.TagSet
}

// ReactiveServices is the former name of KernelServices, kept as an alias so the rename is
// not a breaking change for downstream code mid-migration.
//
// Deprecated: use KernelServices. The bundle is handed to every plugin, not only reactive.
type ReactiveServices = KernelServices

// ReactiveJournal is the durable-execution surface for the reactive lane
// (REACT-01 / ADR-0061). Implemented by the OSS bbolt-backed decorator and injected
// into the premium ReactiveEngine, which stays free of kernel internals. The engine
// treats a nil ReactiveJournal as "durability off" (pure in-memory fan-out).
type ReactiveJournal interface {
	// AppendSignal durably records a signal BEFORE condition evaluation and returns
	// its monotonic sequence number. ttl bounds how long the record is replay-eligible.
	AppendSignal(sig domain.Signal, ttl time.Duration) (seq uint64, err error)
	// ReplayFrom returns journaled signals with seq strictly greater than afterSeq.
	ReplayFrom(afterSeq uint64) ([]domain.JournaledSignal, error)
	// GetCursor returns the last-acked journal seq for a watch (0 if none).
	GetCursor(watchID string) (uint64, error)
	// SetCursor advances a watch's ack cursor.
	SetCursor(watchID string, seq uint64) error
	// MarkExecutedOnce is the exactly-once primitive: it returns true only the first
	// time key is seen (atomic check-and-set), false on every replay/retry thereafter.
	MarkExecutedOnce(key string) (firstTime bool, err error)
	// RecordDeadLetter persists an undeliverable action or an expired signal.
	RecordDeadLetter(dl domain.ReactiveDeadLetter) error
	// ListDeadLetters returns dead-letter entries newest-first (limit <= 0 ⇒ all).
	ListDeadLetters(limit int) ([]domain.ReactiveDeadLetter, error)
	// Prune drops journal records at/below minAcked whose TTL has expired. Returns
	// the count removed.
	Prune(minAcked uint64) (removed int, err error)
}

// ReactiveAgentDispatcher is the agent-manager surface reactive needs:
// direct dispatch (DirectDispatcher) + daemon lifecycle (DaemonLifecycle).
type ReactiveAgentDispatcher interface {
	CallAgent(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error)
	SpawnDaemon(agentID, streamID string, params map[string]any) (instanceID string, err error)
	StopDaemon(streamID string) error
	// CallDaemon routes a handoff to the specific per-stream daemon instance spawned for
	// streamID (ADR-0080), not any instance of the agent. Used by the chat manager to deliver
	// a turn to that conversation's own supervised session daemon.
	CallDaemon(ctx context.Context, streamID string, h *domain.Handoff) (*domain.Handoff, error)
}

// ReactiveMemoryWriter ingests signal content into LTM asynchronously.
type ReactiveMemoryWriter interface {
	ProcessAndStoreAsync(ctx context.Context, text string, sourceAgent string)
}

// ReactivePlanner generates an execution plan for start_plan actions.
type ReactivePlanner interface {
	GetExecutionPlan(ctx context.Context, input string) (*domain.ExecutionPlan, error)
}

// ReactiveWatchStore is the WatchConfig persistence surface (satisfied by the BBolt
// AgentRepoDecorator).
type ReactiveWatchStore interface {
	WriteWatchConfig(cfg domain.WatchConfig) error
	ReadWatchConfig(id string) (domain.WatchConfig, error)
	ReadAllWatchConfigs() ([]domain.WatchConfig, error)
	DeleteWatchConfig(id string) error
	SetWatchConfigActive(id string, active bool) error
}

// DefaultOptions returns the OSS defaults: identity trace wrapper, no agent-call
// logging, no reactive engine (the Watcher is used). Premium overrides these.
func DefaultOptions() Options {
	return Options{
		TraceWrapper: func(g domain.Generator, _ string) domain.Generator { return g },
	}
}
