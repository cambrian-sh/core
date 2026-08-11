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

	// IdentityResolver answers "who is this external sender?" on the inbound path
	// (contract 0077).
	//
	// nil means the surface IS the identity: a policy links to "chat:telegram", so
	// every sender who finds the bot has the same reach and "@unknown_4471" is
	// governed by the same rule as a named colleague — not because they are
	// different people, but because nobody is.
	IdentityResolver domain.IdentityResolver

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

	// AgentGRPCServices mounts premium-owned gRPC services on the AGENT plane
	// (ADR-0118 D3), keyed by fully-qualified gRPC service name (e.g.
	// "cambrian.premium.substrate.SubstrateRetrieval"). Declaring the name is
	// what routes auth: these services are exempt from the operator bearer,
	// stamped SurfaceAgent, and have the caller principal seeded from
	// x-agent-id metadata by the kernel's interceptors — exactly as trustworthy
	// as the rest of the agent plane (the SEC-03 residual applies equally). No
	// premium service name is ever hardcoded in OSS; the declaration IS the
	// registration. OSS default: nil. Plugins contribute through
	// Registry.AddAgentGRPCService.
	AgentGRPCServices map[string]func(*grpc.Server)

	// SubstrateConsultant, when non-nil, is consulted by the fact-lane retrieval
	// path AFTER final assembly (ADR-0118 D5): it answers the modelled part of a
	// query exactly through the SCOPED substrate seam and returns citations that
	// ride a synthetic, non-displacing result row. Any error or refusal fails
	// open to the ordinary answer. OSS default: nil — the call site is a nil
	// check and kernel behaviour is bit-identical. Plugins contribute through
	// Registry.SetSubstrateConsultant.
	SubstrateConsultant domain.SubstrateConsultant

	// DecisionObserver, when non-nil, receives a value-copy record of every completed
	// retrieval (ADR-0103 D3). It observes AFTER assembly and cannot influence, delay
	// or fail a query — retrieval reads fail open, and a provenance lane must not be
	// able to invert that. OSS default: nil, which leaves the emit site a nil check
	// and kernel behaviour bit-identical. Plugins contribute through
	// Registry.AddDecisionObserver; this field is the directly-set equivalent.
	DecisionObserver domain.DecisionObserver
	// AgentActivityObserver receives agent tool activity (which tool, which args,
	// and its outcome) keyed by the caller's session token. Operator-facing and
	// append-only; see domain.AgentActivity for why it is not ProgressUpdate.
	AgentActivityObserver domain.AgentActivityObserver

	// EvidenceTransformers are the transformation-stage consumers of the
	// evidence outbox (ADR-0108 D3). Plugins contribute through
	// Registry.AddEvidenceTransformer; empty means the outbox consumer never
	// starts, which is correct — a consumer with no consumers is the unwired
	// trap with a ticker.
	EvidenceTransformers []domain.EvidenceTransformer

	// KnowledgeKinds and ResolutionAuthorities feed the ADR-0110 kind
	// registry, validated as a unit at boot: a malformed spec, a duplicate
	// kind, or a policy nobody registered refuses to start rather than
	// falling back silently.
	KnowledgeKinds        []domain.KindSpec
	ResolutionAuthorities []domain.ResolutionAuthority
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
	Dispatcher domain.StepDispatcher   // selection + invocation (the Dispatcher)
	// Ingestor is the full-fidelity memory write (ADR-0104 D3): one write path, so
	// a lane that receives content puts it in the brain rather than beside it.
	// nil ⇒ this deployment has no ingestion pipeline, and a caller must say so
	// rather than silently detecting over content nobody stored.
	Ingestor ReactiveMemoryIngestor
	// Documents resolves a reference to its content, scope-enforced by the kernel.
	// nil ⇒ this deployment cannot resolve references, and an action that needs one
	// must say so rather than proceeding without evidence.
	Documents     ReactiveDocumentReader
	Planner       ReactivePlanner       // plan generation for start_plan actions
	LLM           domain.Generator      // LLM condition evaluation
	WatchStore    ReactiveWatchStore    // WatchConfig persistence (BBolt) — being retired
	PipelineStore ReactivePipelineStore // authored pipeline persistence (BBolt)
	EventBus      domain.EventBus       // daemon-crash subscription, emit_event
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

	// Agents lists registered agents, so a plugin can tell a real principal from
	// one that only a policy link mentions (contract 0074 ListPrincipals).
	//
	// Read-only by construction — the port carries GetAllAgents and nothing else,
	// because a plugin that could MUTATE the registry could mint principals for
	// itself, and the whole value of the orphan check is that it compares against
	// a set the plugin does not control.
	Agents domain.AgentLister

	// RegisterAgent adds ONE agent definition to the live registry, so a plugin
	// that gains a new unit at runtime does not need a restart to run it.
	//
	// This is a deliberate narrowing of the read-only rule above, not a
	// repeal of it. The seam is bounded at construction, in buildPlugins:
	//
	//   - the id must sit in the plugin's OWN namespace ("<plugin-id>_…"), so a
	//     plugin cannot mint a principal that policy elsewhere refers to;
	//   - System is forced false, so no privilege can be granted this way.
	//
	// What it deliberately does NOT do is register an INGRESS. A new agent has
	// no surface until an operator registers one on the access plane (ADR-0090
	// D2), so the reach a plugin can grant itself here is exactly none: it can
	// create a unit that runs, not a door that traffic arrives through.
	//
	// nil in tests and in any kernel built without a registry — a plugin must
	// degrade to "this takes effect on the next start" rather than panic.
	RegisterAgent func(domain.AgentDefinition) error

	// RegisterPipelineLister contributes the operator console's reactive-pipeline
	// read surface (contract 0087, ADR-0114 D33/D34).
	//
	// A plugin registers rather than the kernel reaching into it, because the
	// authored pipelines live in the plugin that authors them and the console has
	// no business knowing that package exists.
	//
	// nil in a kernel built without an operator plane — a plugin must degrade to
	// "nothing is listed" rather than panic.
	RegisterPipelineLister func(domain.PipelineLister)

	// RegisterPipelineDryRunner contributes the shadow-run surface (contract
	// 0088).
	//
	// Separate from the lister, and singular where that one aggregates. Listing
	// is a union — every source contributes rows and the console shows them all.
	// A dry run names ONE pipeline and asks what it would do, so a second
	// contributor could only answer about a pipeline it does not own. Last
	// registration wins, and in practice only the reactive plugin registers one.
	//
	// nil in a kernel built without one — the RPC then refuses by name rather
	// than reporting that nothing would happen.
	RegisterPipelineDryRunner func(domain.PipelineDryRunner)

	// RegisterPipelineAuthor contributes the canvas read surface (contract
	// 0089): read one authored revision, and compile a graph without storing it.
	//
	// Singular for the same reason as the dry runner — both name ONE pipeline,
	// where listing is a union over every source.
	//
	// nil in a kernel built without one — the RPCs then refuse by name rather
	// than answering with an empty graph a canvas would draw.
	RegisterPipelineAuthor func(domain.PipelineAuthor)

	// RegisterPipelineWriter contributes the draft-save surface (contract 0090).
	//
	// Separate from RegisterPipelineAuthor, matching the separate port: the read
	// surface's guarantee is that it cannot write, and a build can legitimately
	// have one without the other.
	RegisterPipelineWriter func(domain.PipelineWriter)

	// RegisterPipelineLifecycle contributes the transition surface (contract
	// 0093): draft → validated → published → armed, and back to published,
	// which is the pause. Separate from the writer for the same reason the
	// writer is separate from the author — this surface cannot edit a graph,
	// only move a revision through gates the registry already holds.
	RegisterPipelineLifecycle func(domain.PipelineLifecycle)

	// RegisterIngressLister contributes the list of ADR-0090 registered
	// ingresses, so surfaces that describe ingresses can name them all rather
	// than only the ones a particular plugin authored.
	//
	// nil in a kernel with no registry — a plugin degrades to "none registered".
	RegisterIngressLister func(domain.IngressLister)

	// RegisterIngressSchemaDeclarer contributes the schema half of the
	// ADR-0090 registry (ADR-0117): recording what a registered ingress's
	// items carry, so a plugin that owns an entry point can declare its
	// payload contract instead of the deployment inferring it from captures.
	RegisterIngressSchemaDeclarer func(domain.IngressSchemaDeclarer)

	// RegisterIngressDeregistrar contributes the WRITE half of the ADR-0090
	// registry — withdrawing an entry organ when the plugin that owns it removes
	// it. Separate from the lister for the same reason the interfaces are
	// separate: listing and withdrawing are different powers.
	RegisterIngressDeregistrar func(domain.IngressDeregistrar)

	// RegisterIngressRegistrar contributes the CREATE half of the ADR-0090
	// registry. It existed only as an admin RPC, so a deployment could arm five
	// ingresses and register none — every surface then falls back to
	// transport-derived and no policy can be scoped to an entry point at all.
	// The absence is invisible: the registry looks correctly empty.
	RegisterIngressRegistrar func(domain.IngressRegistrar)

	// RegisterIngressPipelineRetirer contributes the removal of a pipeline armed
	// for an entry organ that has been withdrawn.
	RegisterIngressPipelineRetirer func(func(ctx context.Context, agentID string) error)

	// RegisterTurnRouter contributes the seam that shapes what happens around an
	// admitted conversational turn.
	//
	// Reached only AFTER admission — the ingress daemon authenticated the sender
	// at its external surface, and the kernel has already applied the namespace,
	// the identity binding, blocked senders and the stranger policy. A router
	// cannot see or skip any of it (ADR-0090 D2).
	//
	// nil ⇒ turns run directly, which is every deployment's behaviour today.
	RegisterTurnRouter func(domain.TurnRouter)

	// IngressTraffic reports who has come through an entry point, from the
	// durable conversation record.
	//
	// The console's "recent inbound" list was built from the access-decision
	// journal, which cannot answer the question it asks: the journal is in-memory
	// and only records when something ASKS the decision point, so a turn that
	// answers a greeting leaves no trace. The panel was empty for exactly the
	// traffic it existed to show. Decisions remain the right source for what
	// POLICY did, and ride along as an overlay.
	IngressTraffic domain.IngressTrafficLister

	// Ingresses is the registered entry-point registry, READ-ONLY.
	//
	// A plugin that mints entry points needs to know whether the surface its
	// traffic will arrive on has actually been registered. A bot with no
	// registration is not broken — it polls, it receives, it answers — but its
	// traffic arrives with NO POLICY attached to the door it came through, and
	// that is the one state a console must never render as healthy.
	//
	// Read-only by construction: registering an ingress MINTS A SURFACE and stays
	// an operator action on the access plane (ADR-0090 D2), because a plugin that
	// could grant itself a surface could widen its own reach.
	Ingresses domain.IngressResolver

	// SQL is the kernel's Postgres pool, handed to plugins that own their own
	// tables (the policy plugin's agent_scopes / policy objects). nil when no
	// Postgres is configured — a plugin must degrade rather than panic.
	//
	// It is deliberately the concrete pool rather than an interface: a plugin that
	// owns tables owns their schema and their queries, and pretending otherwise
	// would mean re-exporting half of pgx through a seam nobody benefits from.
	// SetProgressSink installs the ADR-0098 progress channel onto the chat lane.
	// A plugin calls this during Build to start receiving "what is happening now"
	// snapshots; without one the kernel's emission sites are inert.
	//
	// Handed over as a function rather than the service itself so a plugin gets the
	// ONE capability it needs and no access to the turn path around it.
	// nil when the chat lane is not wired (no conversation store, no pool).
	SetProgressSink func(domain.ProgressSink)

	// DeliverProgress sends one supersedable progress snapshot to whatever ingress
	// carries a conversation (ADR-0098 D2). It resolves and re-authorises the address
	// itself, so a plugin never handles delivery addresses.
	//
	// Best-effort: callers are expected to drop the error. nil when there is no
	// ingress delivery path.
	DeliverProgress func(ctx context.Context, conversationID, text string, final bool) error

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

	// Knowledge is the substrate's typed item/resolution boundary (ADR-0106).
	// A plugin that produces or consumes knowledge items goes through THIS port
	// — never SQL against substrate tables, which is the retrofit debt the
	// boundary exists to prevent (memo §18 phase-2 note). nil when no Postgres
	// is configured.
	Knowledge domain.KnowledgeStore

	// DeregisterIngress removes an entry organ's registration (ADR-0090).
	//
	// A plugin that OWNS an entry point — a Telegram bridge, say — must be able
	// to withdraw it when the operator deletes it. Without this, deleting a bot
	// stopped its daemon and forgot its token while leaving the registration
	// behind, so the console kept listing an armed pipeline for a bot that no
	// longer existed and nothing in the system disagreed.
	//
	// nil in a build with no ingress registry: a plugin then degrades to "the
	// registration outlives the bot", which is what shipped.
	DeregisterIngress func(ctx context.Context, agentID string) error

	// DeclareIngressSchema records what an ingress's items carry (ADR-0117),
	// on its EXISTING registration. The plugin that owns the entry point is
	// the one party that knows its payload contract a priori — a Telegram
	// bridge does not need fifty captures to learn it forwards `text`. Errors
	// name their constraint (an unregistered agent among them); nil-safe in a
	// build with no registry, where declaring is a no-op rather than a fault.
	DeclareIngressSchema func(ctx context.Context, agentID string, fields []domain.IngressSchemaField) error

	// RegisterIngress declares an entry organ, at the moment it becomes one.
	//
	// Arming is that moment: it is when the outside world can first reach the
	// ingress, and it is already an operator-gated act, so no new authority is
	// created by registering there. What the caller must NOT do is name an
	// arbitrary surface — an ingress registers itself under a surface derived
	// from its own id, or a new entry point could be dressed in an existing
	// one's policy.
	//
	// nil-safe in a build with no registry, where registering is a no-op rather
	// than a fault — the same degradation as its three siblings.
	RegisterIngress func(ctx context.Context, reg domain.IngressRegistration) error

	// RetireIngressPipeline removes the pipeline armed for an entry organ that
	// has been removed.
	//
	// Paired with DeregisterIngress rather than folded into it: the registration
	// and the pipeline are owned by different plugins, and a single call would
	// make one of them responsible for the other's storage.
	RetireIngressPipeline func(ctx context.Context, agentID string) error

	// TracePipelinePayloads mirrors execution.llm.trace_pipeline_payloads so a
	// plugin can honour it without reading kernel config directly.
	TracePipelinePayloads bool

	// PipelineDrainerEnabled mirrors execution.pipelines.drainer_enabled.
	//
	// Off by default, and deliberately not a side effect of upgrading: turning a
	// drainer on for the first time does not resume a paused system, it starts
	// one. Whatever is already queued executes immediately, against the plan
	// revision each run was PINNED to rather than any correction made since —
	// which on a deployment that had never drained meant hundreds of runs, some
	// belonging to an ingress that had been deleted.
	PipelineDrainerEnabled bool

	// Events is the substrate's typed event/observation boundary (ADR-0108 D2):
	// point lookups and history over stored rows, exact, nothing embedded.
	// nil when no Postgres is configured.
	Events domain.EventStore

	// EvidenceStore READS preserved evidence rows.
	//
	// The write half and the content fetch were both exposed; the row itself was
	// not, so a plugin holding an EvidenceID had no way to reach its content
	// hash — which is what turns an id into bytes. The outbox consumer does
	// exactly this walk internally; a lane that projects outside the outbox needs
	// the same one. nil when evidence capture is disabled.
	EvidenceStore domain.EvidenceStore

	// EvidenceIngest preserves one delivery as evidence under the ADR-0105
	// ordering contract (bytes → verify → atomic evidence+outbox). nil when
	// evidence capture is disabled — a plugin lane that needs the archive must
	// say so rather than silently detecting over content nobody preserved.
	EvidenceIngest func(ctx context.Context, raw domain.RawEvidence) (domain.EvidenceID, bool, error)

	// QueryKnowledge executes one closed knowledge query (ADR-0111) AS the given
	// principal (ADR-0118 D1). The seam takes a principal, never a predicate — a
	// seam accepting a *TagPredicate would let a plugin choose its own access
	// scope, which is a bypass with extra steps (the Documents precedent). Scope
	// resolves inside the kernel via the effective Authorizer: the OSS default
	// (AllowAllAuthorizer) reads unrestricted, a policy authorizer fails closed
	// (domain.ErrQueryDenied when the principal holds no read predicate).
	// "Cannot express safely" stays the only in-AST failure mode. This replaced
	// the raw QueryPlane seam, which was the substrate's one unguarded read
	// path. nil when no Postgres is configured.
	QueryKnowledge func(ctx context.Context, principal domain.PrincipalRef, q domain.KnowledgeQuery) (domain.QueryResult, error)

	// StageEvidenceContent makes one delivery's ORIGINAL bytes durable in the
	// content-addressed store before anything else touches them (ADR-0112 §6).
	// The raw-delivery lane sends the returned CID — never the body — through
	// the signal journal (ADR-0104's payload-as-reference rule), and the
	// ingest_raw action re-presents the bytes to EvidenceIngest, whose own Put
	// is idempotent under the same CID. nil when evidence capture is disabled —
	// a transport that needs the archive must refuse deliveries rather than
	// acknowledge what nothing preserved.
	StageEvidenceContent func(ctx context.Context, data []byte) (domain.CID, error)

	// FetchEvidenceContent resolves a staged CID back to the original bytes —
	// the read half of the raw-delivery lane. nil when evidence capture is
	// disabled.
	FetchEvidenceContent func(ctx context.Context, cid domain.CID) ([]byte, error)

	// ResolveNamedSecret reads ONE named credential (e.g. "ingress:<name>:secret",
	// the llm generator-key naming pattern) from the ADR-0101 store. ok=false
	// when no store is configured or the name is unset. The value must never
	// appear in specs, logs, previews, or errors — the caller holds it exactly
	// long enough to use it. Deliberately name-at-a-time with no list operation:
	// a plugin can use a credential it knows the name of, never enumerate the
	// deployment's secrets.
	ResolveNamedSecret func(name string) (value string, ok bool)

	// GeneratorForModel returns a Generator that PREFERS the named generator
	// id per call, falling down the failover ladder when it is unhealthy or
	// unknown (ADR-0112 §15). Resolution happens on every Generate, so an
	// operator's model change needs no restart. nil when no LLM provider is
	// configured; an empty id behaves as the deployment default.
	GeneratorForModel func(modelID string) domain.Generator

	// StoreNamedSecret / ClearNamedSecret / NamedSecretStatus are the WRITE
	// half of the named-credential seam (ADR-0112 §13): encrypted set, clear,
	// and presence + last-four — never a read-back RPC anywhere above them.
	// The same name-at-a-time discipline: no enumeration. nil-safe like the
	// read half (errors when the store is off).
	StoreNamedSecret  func(name, value string) error
	ClearNamedSecret  func(name string) error
	NamedSecretStatus func(name string) (configured bool, lastFour string)
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
	// Prune drops up to limit journal records at/below minAcked whose TTL has
	// expired. Returns the count removed and whether more remained at the cap
	// (limit <= 0 ⇒ unbounded).
	//
	// Bounded since GOV-02: an unbounded first prune over a journal that has grown
	// for months holds the store's write lock for the whole pass, which is itself
	// the outage the GC exists to prevent.
	Prune(minAcked uint64, limit int) (removed int, more bool, err error)
	// RetainedWindow reports the oldest and newest seq the journal still holds and
	// how many records remain. Pruning shortens what a backtest can replay, so the
	// window must be reportable rather than assumed.
	RetainedWindow() (oldestSeq, newestSeq uint64, count int, err error)
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
	// DaemonRunning reports whether a daemon is spawned for streamID.
	//
	// The console needs it to tell "armed" from "armed, and its entry organ is
	// switched off" — which look identical from the pipeline store, because the
	// graph is armed either way.
	DaemonRunning(streamID string) bool
}

// ReactiveDocumentReader resolves a document REFERENCE to its content, for a
// plugin (ADR-0104 D6.2).
//
// A watch action's signal carries references only — the ingress contract forbids
// bodies in the journal — so an action that needs the content must resolve the
// reference. This is how it asks.
//
// # It takes a PRINCIPAL, never a predicate
//
// The plugin says who it is; the KERNEL decides what that principal may see, by
// running the same `Authorizer.ReadFilter` chokepoint every other read goes
// through. A seam that accepted a `*TagPredicate` would let a plugin choose its
// own access scope, which is not an extension point but a bypass.
//
// This is also why the plugin does not simply hold the document store. It writes
// to memory through KernelServices rather than touching a store, and reading is
// symmetric: capabilities, not handles. A plugin holding a store would enforce
// scope in a second place, and a second copy of the access model is a second thing
// to drift.
type ReactiveDocumentReader interface {
	// GetDocument returns the document, or domain.ErrDocumentNotFound when it does
	// not exist OR this principal cannot see it — deliberately the same answer, so
	// a denial does not confirm existence.
	GetDocument(ctx context.Context, principal domain.PrincipalRef, id string) (domain.Document, error)
}

// ReactiveMemoryIngestor is the FULL-FIDELITY memory write for a plugin
// (ADR-0104 D3).
//
// # Why the thin seam was not enough (and is now gone)
//
// The predecessor seam was `ReactiveMemoryWriter.ProcessAndStoreAsync(ctx, text,
// sourceAgent)` — text and one string. It carried no tags, so a plugin writing through
// it silently stripped the source's classification, and a commitment distilled from
// restricted material would land unrestricted (ADR-0095 D9 / D4 of this ADR). For the
// reactive `ingest` ACTION that was a known thinness; for a lane that writes customer
// conversation it was a classification hole. Every caller having moved here, that seam
// and the LLM-importance write path behind it have been REMOVED — this is now the only
// plugin-facing memory write.
//
// # Why a plugin needs this at all
//
// D3: an ingress reports ONCE and the kernel routes. Before it, the drift lane and
// the memory lane were parallel universes — `IngestMessages` detected and dropped
// the text, while the memory lane was a SEPARATE call the caller had to remember to
// make. A connector calling only the drift RPC produced alerts over an empty
// memory: nothing retrievable, no chunks, no structure, no extracted entities.
//
// It returns the entity id the kernel assigned, which is what makes a
// references-only signal resolvable afterwards.
type ReactiveMemoryIngestor interface {
	// Ingest runs one document through the STANDARD ingestion pipeline — the same
	// path the agent-plane IngestMemory RPC uses — and returns the source-document
	// entity id.
	Ingest(ctx context.Context, doc domain.ExternalDocument) (entityID string, err error)
}

// ReactivePlanner generates an execution plan for start_plan actions.
type ReactivePlanner interface {
	GetExecutionPlan(ctx context.Context, input string) (*domain.ExecutionPlan, error)
}

// ReactiveWatchStore is the WatchConfig persistence surface (satisfied by the BBolt
// AgentRepoDecorator).
// ReactivePipelineStore persists authored reactive pipelines.
//
// The spec is opaque bytes: the pipeline schema is premium's, and a copy of it
// in OSS would be a second definition to keep in step. Core supplies durability.
//
// There is no SetActive: a pipeline's lifecycle state lives inside its spec, so
// a separate flag would be a second place that could disagree with the graph
// about whether it is live.
type ReactivePipelineStore interface {
	WritePipeline(id string, spec []byte) error
	ReadAllPipelines() (map[string][]byte, error)
	DeletePipeline(id string) error
}

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
