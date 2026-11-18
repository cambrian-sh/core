package network

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// LLMGateway is the central control point for ADR-0018 managed LLM calls.
// It owns the lease store, the CONWIP semaphore, lease admission against the
// session spend ceiling, and lease metering. Model health is NOT its concern: since
// ADR-0139 wave 1c every call is served through the Provider's stack (StepModels),
// whose circuit breaker is the only health authority.
type LLMGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error)
	Complete(ctx context.Context, leaseID domain.LeaseID) (llm.TokenUsage, error)
	EvictExpired()
	// StreamChunks serves one streamed C1 request under a lease. The final chunk
	// forwarded to out carries the complete GenerateResponse (StreamChunk.Response:
	// Ignored, usage, stop reason, model) when the call ended normally.
	StreamChunks(ctx context.Context, leaseID domain.LeaseID, req domain.GenerateRequest, out chan<- domain.StreamChunk) error
	// GenerateWithTools runs ONE managed C1 turn, normally with native tool-calling
	// (ADR-0097 Phase B). Returns ErrToolCallingUnsupported when the serving model
	// cannot call tools, which callers translate into the prompt-encoded fallback
	// rather than a failure.
	GenerateWithTools(ctx context.Context, leaseID domain.LeaseID, req domain.GenerateRequest) (domain.GenerateResponse, error)
}

// StepModels is the gateway's view of the LLM Provider (ADR-0139 D1: one pathway,
// one breaker). *llm.Provider implements it. candidates are generator ids in
// preference order; the implementation serves the call on the first one its breaker
// admits and falls back down the list on endpoint-class failures only (transport,
// 429, 5xx) — never on a refusal, a schema failure or an unsupported field. It
// returns the id of the generator that served the call. It must NOT meter: the
// gateway books lease calls itself.
type StepModels interface {
	GenerateOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (domain.GenerateResponse, string, error)
	StreamOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (<-chan domain.StreamChunk, string, error)
}

var _ StepModels = (*llm.Provider)(nil)

// ObservedStepModels is the optional StepModels capability the native tool lane
// streams through (ADR-0139 amendment 2026-11-06): GenerateOn served over the
// generator's stream, reporting deltas to observe while the call runs. A
// StepModels without it serves the turn unary, with no deltas.
type ObservedStepModels interface {
	GenerateOnObserved(ctx context.Context, candidates []string, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (domain.GenerateResponse, string, error)
}

var _ ObservedStepModels = (*llm.Provider)(nil)

// ObservedToolGateway is the optional LLMGateway capability the GenerateWithTools
// RPC uses to forward a turn's live output to the step-output stream while the
// RPC itself stays unary.
type ObservedToolGateway interface {
	GenerateWithToolsObserved(ctx context.Context, leaseID domain.LeaseID, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (domain.GenerateResponse, error)
}

var _ ObservedToolGateway = (*SubstrateLLMGateway)(nil)

// SubstrateLLMGateway implements LLMGateway.
type SubstrateLLMGateway struct {
	mu        sync.RWMutex
	sessions  map[domain.LeaseID]*domain.BudgetLeaseState
	semaphore chan struct{}
	cfg       config.ExecutionConfig
	// models serves every call (ADR-0139 D1): the Provider's breaker, tracing,
	// concurrency cap and schema layer. Attached at composition, before serving.
	models   StepModels
	Observer domain.TelemetryObserver // ADR-0019: may be nil (no op)
	// defaultModelID is the LAST rung of every step's model chain: winner →
	// fallbacks → default. With no allocated winner it is the only rung, so an agent
	// step still runs instead of failing — the analogue of the Provider ladder's
	// global default. An agent id ("llm:<generator>"). Empty ⇒ no rung.
	defaultModelID string

	// meter is the ADR-0131 D1 spend ledger — the SAME object the Provider meters
	// the organs with, so agent streams and organ calls add up in one place.
	// Set once by NewLLMGateway and never nil (ENG-1504).
	meter atomic.Pointer[llm.SpendMeter]

	// onComplete runs after a lease is completed and removed. It is the ONE place
	// "lease finished" side effects hang, so a new lease site (the retrieval
	// dispatcher was the third) cannot forget them: today it clears the ADR-0046
	// D6 run-scoped skill grants keyed by the lease
	// (T-20261101-retrieval-dispatch-lease-retrieval-dispatch-go).
	onComplete atomic.Pointer[func(domain.LeaseID)]

	// agentModel resolves the model preference of the agent a lease is bound to
	// (agent stack v2: a declarative profile's `model:`, a generator id or the
	// generator behind a role, resolved per call so a role reassignment is live).
	// The preferred generator becomes the FIRST rung of that lease's chain, ahead of
	// the step allocation and the default: a prior, never a command — an unknown or
	// unhealthy id is skipped by the Provider's chain walk and the next rung serves.
	// Nil, or "" for an agent, ⇒ the chain is unchanged.
	agentModel atomic.Pointer[func(agentID string) string]
}

// SetAgentModelPreference attaches the per-agent model preference resolver (see
// agentModel). It returns a generator id, or "" for no preference.
func (g *SubstrateLLMGateway) SetAgentModelPreference(fn func(agentID string) string) {
	if fn == nil {
		g.agentModel.Store(nil)
		return
	}
	g.agentModel.Store(&fn)
}

// preferredModel is the generator the lease's bound agent prefers, or "".
func (g *SubstrateLLMGateway) preferredModel(ss *domain.BudgetLeaseState) string {
	fn := g.agentModel.Load()
	if fn == nil || ss == nil || ss.Binding.AgentID == "" {
		return ""
	}
	return (*fn)(ss.Binding.AgentID)
}

// SpendMeter returns the attached spend ledger, or nil.
func (g *SubstrateLLMGateway) SpendMeter() *llm.SpendMeter { return g.meter.Load() }

// SetCompleteHook registers the function Complete runs for every completed
// lease, after the lease is removed. Nil clears it.
func (g *SubstrateLLMGateway) SetCompleteHook(fn func(domain.LeaseID)) {
	if fn == nil {
		g.onComplete.Store(nil)
		return
	}
	g.onComplete.Store(&fn)
}

// SetDefaultModelID configures the fallback streaming model used when a step has
// no allocated model winner. Pass the configured default generator's agent id
// ("llm:<default>"). Empty disables the fallback (the prior fail-hard behavior).
func (g *SubstrateLLMGateway) SetDefaultModelID(modelID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.defaultModelID = modelID
}

// NewLLMGateway creates a SubstrateLLMGateway. The composition root attaches its
// StepModels (SetStepModels) before any lease is used.
//
// meter is the ADR-0131 spend ledger: every agent stream and tool turn is admitted
// against the session ceiling (execution.session.max_session_tokens) and booked
// there. It is a REQUIRED constructor argument (ENG-1504, AGENTS.md §2.3): it used to
// be attached afterwards through a setter the composition root reached with an
// anonymous-interface assertion, so a gateway type that stopped matching ran every
// agent-stream call with no ceiling, silently. A gateway without a meter is now a
// compile error, and a nil meter is refused here (panic at boot). A deployment with
// no ceiling still passes a meter — llm.NewSpendMeter(0, …) meters and never refuses.
func NewLLMGateway(cfg config.ExecutionConfig, meter *llm.SpendMeter) *SubstrateLLMGateway {
	if meter == nil {
		panic("network.NewLLMGateway: nil spend meter (ADR-0131: the session spend ceiling must reach every agent-stream call)")
	}
	g := &SubstrateLLMGateway{
		sessions:  make(map[domain.LeaseID]*domain.BudgetLeaseState),
		semaphore: make(chan struct{}, cfg.LLM.LLMGatewayMaxConcurrency),
		cfg:       cfg,
	}
	g.meter.Store(meter)
	return g
}

// SetStepModels attaches the model source every lease call is served through — the
// kernel's *llm.Provider in production. A live generator reload needs no call here:
// the Provider swaps its own table.
func (g *SubstrateLLMGateway) SetStepModels(m StepModels) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.models = m
}

// minSessionTTL floors a session's initial lifetime. The TTL is a LEAK safety-net (the
// per-step deferred Complete does the real cleanup), not a step-timer — so it must be
// long enough that a live step is NEVER swept mid-flight. The session is Acquired at step
// DISPATCH, before the (possibly slow, multi-LLM) auction + seed-recall that run before
// the agent's first generate; a short TTL (estimatedDuration was a hardcoded 30s → 150s)
// can expire in that gap, so EvictExpired deletes the live session and the agent's next
// GenerateViaModelStream fails "session not found". The keepalive then refreshes a rolling
// window once streaming starts; this floor only has to cover dispatch→first-generate.
const minSessionTTL = 10 * time.Minute

// StepLeaseTokenLimit and StepLeaseDuration size the lease an agent step is
// dispatched with (ADR-0018: the token limit drives the Pass-1 cut at 90%; the
// duration is the estimate the TTL is derived from, floored at minSessionTTL).
// Named once so a lease that must be sized "like a step" — the verifier call's —
// reuses these instead of restating the literals.
const (
	StepLeaseTokenLimit = 4096
	StepLeaseDuration   = 30 * time.Second
)

// leaseIDBytes is the lease id's entropy: 128 bits (ADR-0139 D8).
const leaseIDBytes = 16

// newLeaseID mints a lease id from crypto/rand. A lease is a bearer credential — it
// resolves to a session, a beneficiary, a conversation and a spend budget — so it must be
// unguessable. The previous `lease-<UnixNano>-<math/rand%10000>` had about 13 bits of
// entropy on top of a clock an observer can estimate, and it was clock-derived identity
// (AGENTS.md §2.6). The "lease-" prefix is kept: operators and logs recognise it.
func newLeaseID() (domain.LeaseID, error) {
	var b [leaseIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("llm gateway: mint lease id: %w", err)
	}
	return domain.LeaseID("lease-" + hex.EncodeToString(b[:])), nil
}

// Acquire creates a new session for a step, allocating a session token UUID.
//
// A lease acquired under a domain.WithBackgroundSpend ctx is a background lease
// (ADR-0131, owner decision 2026-11-01): its calls are booked to the session it
// is bound to, never refused at that session's ceiling. The marker is read here,
// from the kernel-side ctx that mints the lease, so no agent can claim it.
func (g *SubstrateLLMGateway) Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error) {
	sessionID, err := newLeaseID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	ttl := time.Duration(float64(estimatedDuration) * g.cfg.Session.SessionTokenTTLMultiplier)
	if ttl < minSessionTTL {
		ttl = minSessionTTL
	}
	g.mu.Lock()
	g.sessions[sessionID] = &domain.BudgetLeaseState{
		StepAllocation: sa,
		TokenLimit:     tokenLimit,
		ExpiresAt:      now.Add(ttl),
		LastActivityAt: now,
		Background:     ctx != nil && domain.IsBackgroundSpend(ctx),
		// The request that minted the lease counts the calls made under it, even
		// though they arrive later over the agent's own RPC (domain.LLMUsageTally).
		Tally: domain.LLMUsageTallyFrom(ctx),
	}
	g.mu.Unlock()
	return sessionID, nil
}

// BindLease attaches execution identity to an already-issued lease (domain.LeaseResolver).
//
// Called by the kernel at dispatch, immediately after Acquire. It is the write half of the
// Phase-0 identity fix: because the kernel — not the agent — records which session a lease
// belongs to, an agent presenting that lease can be resolved to its session without the
// kernel ever trusting an agent-supplied session ID.
//
// A no-op for an unknown or already-completed lease: binding is best-effort metadata, and a
// lease that has been swept must not be resurrected as a map entry.
func (g *SubstrateLLMGateway) BindLease(leaseID domain.LeaseID, b domain.LeaseBinding) {
	if g == nil || leaseID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.sessions[leaseID]; ok {
		st.Binding = b
	}
}

// BindLeaseAgent records the agent a lease has just been dispatched to
// (domain.LeaseAgentBinder), leaving the rest of the binding as BindLease wrote it. The
// dispatcher calls it immediately before every CallAgent, so the ADR-0139 D8 ownership
// check (Server.resolveLeaseFor) applies to plan-step leases, which are minted before
// selection. See domain.LeaseAgentBinder for why the AgentID moves on re-dispatch.
func (g *SubstrateLLMGateway) BindLeaseAgent(leaseID domain.LeaseID, agentID string) bool {
	if g == nil || leaseID == "" || agentID == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.sessions[leaseID]
	if !ok {
		return false
	}
	if prev := st.Binding.AgentID; prev != "" && prev != agentID {
		slog.Debug("lease handed to another agent by dispatch",
			"lease_prev_agent", prev, "lease_agent", agentID, "step", st.Binding.StepIndex)
	}
	st.Binding.AgentID = agentID
	return true
}

// ResolveLease returns the identity bound to leaseID (domain.LeaseResolver).
//
// The bool reports whether the lease is KNOWN, not whether it carries a binding — a live
// but unbound lease returns (zero, true). Transports rely on that distinction: "known
// lease" means the caller is an agent and its header must not be read as a session ID,
// even when no session was bound. Whether the CALLER may use the lease is decided by
// Server.resolveLeaseFor, not here: an agent may use only a lease bound to itself.
func (g *SubstrateLLMGateway) ResolveLease(leaseID domain.LeaseID) (domain.LeaseBinding, bool) {
	if g == nil || leaseID == "" {
		return domain.LeaseBinding{}, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	st, ok := g.sessions[leaseID]
	if !ok {
		return domain.LeaseBinding{}, false
	}
	return st.Binding, true
}

// Complete reads the final reconciled token usage — summed over every call made
// under the lease, provider-reported where the provider reported it (ADR-0131
// D2) — removes the lease, and runs the completion hook.
func (g *SubstrateLLMGateway) Complete(_ context.Context, sessionID domain.LeaseID) (llm.TokenUsage, error) {
	g.mu.Lock()
	ss, ok := g.sessions[sessionID]
	if !ok {
		g.mu.Unlock()
		return llm.TokenUsage{}, fmt.Errorf("session not found: %s", sessionID)
	}
	usage := leaseUsage(ss)
	delete(g.sessions, sessionID)
	g.mu.Unlock()
	// Outside the lock: the hook reaches into other subsystems (the tool
	// executor's grant overlay) and must not run under the gateway's mutex.
	if fn := g.onComplete.Load(); fn != nil {
		(*fn)(sessionID)
	}
	return usage, nil
}

// leaseUsage renders a lease's accumulated counters as a TokenUsage.
func leaseUsage(ss *domain.BudgetLeaseState) llm.TokenUsage {
	return llm.TokenUsage{
		PromptTokens:     ss.PromptTokensUsed,
		CompletionTokens: ss.ActualTokensUsed - ss.PromptTokensUsed,
		TotalTokens:      ss.ActualTokensUsed,
		CacheReadTokens:  ss.CacheReadTokensUsed,
		CacheWriteTokens: ss.CacheWriteTokensUsed,
		Estimated:        ss.UsageEstimated,
		BudgetOverrun:    ss.BudgetOverrun,
	}
}

// addLeaseUsageLocked adds one call's reconciled usage to a lease. Callers hold
// g.mu. Additive, never assigning: an agent's ReAct loop makes many calls under
// ONE step lease, and the lease total is their sum.
func addLeaseUsageLocked(ss *domain.BudgetLeaseState, u llm.TokenUsage) {
	ss.ActualTokensUsed += u.TotalTokens
	ss.PromptTokensUsed += u.PromptTokens
	ss.CacheReadTokensUsed += u.CacheReadTokens
	ss.CacheWriteTokensUsed += u.CacheWriteTokens
	if u.Estimated {
		ss.UsageEstimated = true
	}
}

// ChargeLease adds usage incurred ELSEWHERE to a live lease — the agentic
// retrieval chain runs under its own short-lived lease and is charged back to the
// step lease of the agent whose QueryMemory started it (ADR-0131 D3: "the agentic
// retrieval chain counted against the step's lease"). Unknown leases are ignored:
// the step may already have completed, and charging must never resurrect it.
//
// Charging counts; it does not cut. The Pass-1 cut-off (BudgetOverrun) stays a
// property of the agent's own stream — a 6-way decomposition easily exceeds a
// 4096-token step lease, and failing every such step would turn agentic recall
// off by accident. The hard stop for runaway spend is the session ceiling.
func (g *SubstrateLLMGateway) ChargeLease(leaseID domain.LeaseID, u llm.TokenUsage) {
	if g == nil || leaseID == "" || u.IsZero() {
		return
	}
	g.mu.Lock()
	ss, ok := g.sessions[leaseID]
	if ok {
		addLeaseUsageLocked(ss, u)
	}
	g.mu.Unlock()
	if ok {
	}
}

// leaseSession returns the session a lease is bound to, falling back to the
// session in ctx. The binding is authoritative: it was written by the kernel at
// dispatch, whereas ctx on an agent RPC is whatever the transport resolved.
func leaseSession(ctx context.Context, ss *domain.BudgetLeaseState) domain.SessionID {
	if ss.Binding.SessionID != "" {
		return ss.Binding.SessionID
	}
	sid, _ := domain.SessionIDFromContext(ctx)
	return sid
}

// leasePurpose labels a gateway call for the spend log and the per-request
// tally: "agent_step", or "agent_step:<agent id>" when the kernel bound the lease
// to an agent (e.g. "agent_step:retrieval_agent" for the agentic retrieval
// chain), so a request's report says WHICH agent's calls it paid for. Read under
// g.mu: BindLease writes Binding under it.
func (g *SubstrateLLMGateway) leasePurpose(ss *domain.BudgetLeaseState) string {
	g.mu.RLock()
	agent := ss.Binding.AgentID
	g.mu.RUnlock()
	if agent == "" {
		return string(domain.PurposeAgentStep)
	}
	return string(domain.PurposeAgentStep) + ":" + agent
}

// admitLease is the ADR-0131 D3 session-ceiling check for one call under a
// lease. A background lease is admitted unconditionally — booked to its session
// by the caller's RecordSession, never refused (owner decision 2026-11-01): a
// refusal would silently drop the organ's job (a verification, here).
func admitLease(meter *llm.SpendMeter, ss *domain.BudgetLeaseState, sid domain.SessionID, prompt string) error {
	if ss.Background {
		return nil
	}
	return meter.AdmitSession(sid, string(domain.PurposeAgentStep), llm.EstimateTokens(prompt))
}

// EvictExpired removes all expired session state entries, and lets the spend
// ledger forget sessions idle for longer than the session retention window
// (execution.session.session_retention_days; 0 keeps them for the kernel's life).
func (g *SubstrateLLMGateway) EvictExpired() {
	g.mu.Lock()
	for id, ss := range g.sessions {
		// A lease with a call in flight is live whatever its TTL says: a long
		// unary generation refreshes nothing until it returns.
		if ss.InFlightCalls == 0 && time.Now().After(ss.ExpiresAt) {
			slog.Warn("session_token_leaked", "session_id", id, "agent_id", ss.StepAllocation.Winner.ID)
			delete(g.sessions, id)
		}
	}
	g.mu.Unlock()
	if days := g.cfg.Session.SessionRetentionDays; days > 0 {
		g.meter.Load().EvictIdle(time.Now(), time.Duration(days)*24*time.Hour)
	}
	// ADR-0131: the kernel no-session bucket is reported, never enforced; this
	// periodic sweep is where its accrual reaches the log (nil-safe, silent when
	// nothing accrued).
	g.meter.Load().ReportNoSession()
}

// AddSession adds a pre-existing session state (for testing/setup).
func (g *SubstrateLLMGateway) AddSession(id domain.LeaseID, ss *domain.BudgetLeaseState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[id] = ss
}

// SessionCount returns the current number of active sessions (for testing).
func (g *SubstrateLLMGateway) SessionCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.sessions)
}

// GetBudgetLeaseState returns a copy of the budget-lease state for the given lease ID,
// or nil if not found. Used by GenerateViaModelStream for trace attribution.
func (g *SubstrateLLMGateway) GetBudgetLeaseState(id domain.LeaseID) *domain.BudgetLeaseState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ss, ok := g.sessions[id]
	if !ok {
		return nil
	}
	// Return a shallow copy to avoid callers mutating the shared state.
	cpy := *ss
	return &cpy
}

// leaseCall is what both lease lanes resolve before a model is touched.
type leaseCall struct {
	ss         *domain.BudgetLeaseState
	candidates []string // generator ids: winner → fallbacks → default
	sid        domain.SessionID
	meter      *llm.SpendMeter
	prompt     string // the request text, for admission and the len/4 estimate
	models     StepModels
	// traceAttrs name who the call is for (lease, agent, plan step) on the run
	// trace's model-call spans (ADR-0145 D10), which the Provider records without
	// knowing the lease (domain.WithTraceAttributes).
	traceAttrs map[string]any
}

// leaseTraceAttrs are a lease's identity as run-trace attributes, so the kernel's
// "chat" span of an agent-plane call joins the agent's own spans (same names as
// network/run_trace.go and the SDK's run_trace.py).
func leaseTraceAttrs(lease domain.LeaseID, b domain.LeaseBinding) map[string]any {
	attrs := map[string]any{"cambrian.lease.id": string(lease)}
	if b.AgentID != "" {
		attrs[domain.GenAIAgentName] = b.AgentID
	}
	if b.RunID != "" {
		attrs["cambrian.run.id"] = string(b.RunID)
	}
	if b.StepIndex >= 0 && b.RunID != "" {
		attrs["cambrian.step.index"] = b.StepIndex
	}
	if b.ConversationID != "" {
		attrs[domain.GenAIConversationID] = b.ConversationID
	}
	return attrs
}

// beginLeaseCall runs the guards every managed call shares — lease lookup, the
// step's model chain, and the ADR-0131 D3 ceiling check — so the streaming lane and
// the tool lane cannot drift apart on any of them. The caller holds a CONWIP slot.
func (g *SubstrateLLMGateway) beginLeaseCall(ctx context.Context, sessionID domain.LeaseID, req domain.GenerateRequest) (*leaseCall, error) {
	g.mu.RLock()
	ss, ok := g.sessions[sessionID]
	def := g.defaultModelID
	models := g.models
	var binding domain.LeaseBinding
	if ok {
		binding = ss.Binding // read under the lock: BindLeaseAgent moves AgentID
	}
	g.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	candidates := withPreferredModel(g.preferredModel(ss), stepCandidates(ss.StepAllocation, def))
	if len(candidates) == 0 {
		// An ALLOCATION problem — the auction produced no model candidate and no
		// default is configured — not a health one.
		slog.Error("llm_gateway: no model allocated to this step and no default model configured. " +
			"Check the agent manifest's RequiredModelCapabilities and that a matching TraitModel is registered.")
		return nil, fmt.Errorf("no model allocated for session %s and no default model configured", sessionID)
	}
	if models == nil {
		return nil, fmt.Errorf("llm gateway: no model source wired (SetStepModels)")
	}
	lc := &leaseCall{ss: ss, candidates: candidates, sid: leaseSession(ctx, ss),
		meter: g.meter.Load(), prompt: llm.RequestText(req), models: models,
		traceAttrs: leaseTraceAttrs(sessionID, binding)}
	// ADR-0131 D3: the session ceiling is checked before any model is touched.
	if err := admitLease(lc.meter, ss, lc.sid, lc.prompt); err != nil {
		return nil, err
	}
	return lc, nil
}

// stepCandidates is a step's model chain as generator ids: the auction's winner,
// its fallbacks, then the configured default, without repeats (ADR-0139 D1).
// Allocations and the default name model AGENTS ("llm:<generator id>"); the
// Provider is keyed by generator id.
func stepCandidates(sa domain.StepAllocation, defaultModelID string) []string {
	ids := make([]string, 0, 4)
	add := func(agentID string) {
		id := strings.TrimPrefix(agentID, "llm:")
		if id == "" || slices.Contains(ids, id) {
			return
		}
		ids = append(ids, id)
	}
	add(sa.Winner.ID)
	for _, fb := range sa.Fallbacks {
		add(fb.ID)
	}
	add(defaultModelID)
	return ids
}

// withPreferredModel puts the bound agent's preferred generator first on the
// chain, without repeating it. "" leaves the chain as it is.
func withPreferredModel(pref string, chain []string) []string {
	pref = strings.TrimPrefix(pref, "llm:")
	if pref == "" {
		return chain
	}
	out := make([]string, 0, len(chain)+1)
	out = append(out, pref)
	for _, id := range chain {
		if id != pref {
			out = append(out, id)
		}
	}
	return out
}

// acquireSlot takes a CONWIP slot, or fails fast with gateway_overflow.
func (g *SubstrateLLMGateway) acquireSlot(ctx context.Context) (func(), error) {
	select {
	case g.semaphore <- struct{}{}:
		return func() { <-g.semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, fmt.Errorf("gateway_overflow")
	}
}

// trackCall marks one model call as in flight on the lease until the returned func
// runs (once, when the call returns). It is what makes a long UNARY generation
// visible as liveness: such a call streams nothing, so without the in-flight count
// the chat stall watchdog saw a healthy 60-90 s generation as silence and cancelled
// it (T-20261105-slow-model-reliability-hardcoded-20-s). A counter read on demand,
// not a heartbeat goroutine per call, so a hot path gains no goroutines.
func (g *SubstrateLLMGateway) trackCall(leaseID domain.LeaseID) func() {
	g.mu.Lock()
	if s, ok := g.sessions[leaseID]; ok {
		s.InFlightCalls++
		g.touchLocked(s)
	}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		// Looked up again: Complete may have removed the lease meanwhile.
		if s, ok := g.sessions[leaseID]; ok {
			if s.InFlightCalls > 0 {
				s.InFlightCalls--
			}
			s.LastActivityAt = time.Now()
		}
	}
}

// ConversationActivity reports when model work for a conversation was last seen:
// now while any call is in flight on a lease bound to it (domain.LeaseBinding
// .ConversationID, stamped by the chat lane at turn start), else the latest call
// start, chunk or return on those leases. Zero when no live lease is bound to the
// conversation. It is the gateway's half of the chat liveness probe
// (app.go composes it with the progress holder's LastActivity); a plain lease scan,
// because it is polled a few times per stall window, not per call.
func (g *SubstrateLLMGateway) ConversationActivity(conversationID string) time.Time {
	if conversationID == "" {
		return time.Time{}
	}
	return g.activityWhere(func(b domain.LeaseBinding) bool { return b.ConversationID == conversationID })
}

// RunActivity is ConversationActivity for a plan: the leases bound to runID (every
// step lease of one Execute carries its run id, server.go BindLease). It is the
// gateway's half of the plan idle watchdog (plan_watchdog.go), the same counter
// the chat stall watchdog reads — one liveness mechanism, two consumers
// (T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
func (g *SubstrateLLMGateway) RunActivity(runID domain.RunID) time.Time {
	if runID == "" {
		return time.Time{}
	}
	return g.activityWhere(func(b domain.LeaseBinding) bool { return b.RunID == runID })
}

// activityWhere is the shared lease scan behind ConversationActivity and
// RunActivity: now when a matching lease has work in flight, else the latest
// activity on the matching leases, else zero.
func (g *SubstrateLLMGateway) activityWhere(match func(domain.LeaseBinding) bool) time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	var last time.Time
	for _, s := range g.sessions {
		if !match(s.Binding) {
			continue
		}
		if s.InFlightCalls > 0 {
			return time.Now()
		}
		if s.LastActivityAt.After(last) {
			last = s.LastActivityAt
		}
	}
	return last
}

// TrackLeaseWork marks kernel-side work done for the holder of a lease — a tool
// call (ExecuteTool) or a recall (QueryMemory) — as in flight on that lease until
// the returned func runs, exactly like a model call (trackCall). Those RPCs can
// run for minutes (a page fetch, an agentic retrieval chain) without a model call
// on the lease, and a plan or chat turn doing that is working, not idle. A no-op
// for an empty or unknown lease; the returned func is always safe to call.
func (g *SubstrateLLMGateway) TrackLeaseWork(leaseID domain.LeaseID) func() {
	if g == nil || leaseID == "" {
		return func() {}
	}
	return g.trackCall(leaseID)
}

// touchLocked refreshes a lease's keepalive TTL. Callers hold g.mu.
//
// The keepalive only ever EXTENDS the expiry: it never brings forward the TTL the lease
// was acquired with. A chat turn's lease is acquired for the whole turn (TurnTimeout),
// then goes quiet while the plan it delegated runs on leases of its own; when the
// keepalive overwrote the expiry with now+5 min, a delegation longer than that had its
// turn lease swept as leaked and the chat agent's next call failed "session not found"
// (T-20261109-chat-turn-lease-is-evicted-while-its-delegated).
func (g *SubstrateLLMGateway) touchLocked(s *domain.BudgetLeaseState) {
	ttl := time.Duration(float64(time.Minute) * g.cfg.Session.SessionTokenTTLMultiplier)
	now := time.Now()
	if next := now.Add(ttl); next.After(s.ExpiresAt) {
		s.ExpiresAt = next
	}
	s.LastActivityAt = now
}

// StreamChunks opens a managed stream on the step's model chain (ADR-0018,
// ADR-0139 D1). It applies CONWIP, lease admission, Pass 1 budget enforcement and
// Pass 2 reconciliation. Health and fallback are the Provider's: there is no second
// health cache here.
func (g *SubstrateLLMGateway) StreamChunks(ctx context.Context, sessionID domain.LeaseID, req domain.GenerateRequest, out chan<- domain.StreamChunk) (err error) {
	var streamFault error // a typed error a final stream_error chunk carried
	defer func() {
		if err == nil && streamFault != nil {
			g.recordCallOutcome(sessionID, streamFault)
			return
		}
		g.recordCallOutcome(sessionID, err)
	}()
	start := time.Now()
	release, err := g.acquireSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	if g.Observer != nil {
		g.Observer.OnConwipWait(ctx, time.Since(start).Milliseconds())
	}

	lc, err := g.beginLeaseCall(ctx, sessionID, req)
	if err != nil {
		return err
	}
	defer g.trackCall(sessionID)()
	ctx = domain.WithTraceAttributes(ctx, lc.traceAttrs)

	// The stream is cancelled when this call returns. A Pass-1 cut stops reading
	// early, and the Provider's layers (its concurrency slot, the breaker record)
	// release only once the stream ends, so the cut must end it.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// When a D11 post_model hook matches the step, StreamOn serves the call
	// BUFFERED (llm.Provider.StreamOn): the chunks below are then the complete,
	// post-hook reply, and a hook refusal or a broken stream is an error here with
	// nothing delivered. The Pass-1 cut below cannot stop such a generation early —
	// it sees the reply only once it is complete; the request's max_tokens bounds it.
	chunks, modelID, err := lc.models.StreamOn(sctx, lc.candidates, req)
	if errors.Is(err, domain.ErrHookDenied) {
		slog.Info("llm_gateway: model stream refused by a hook", "model", modelID, "err", err)
		return err
	}
	if err != nil {
		// Surface the REAL cause (HTTP 401, connection refused, bad endpoint).
		slog.Error("llm_gateway: model stream failed",
			"candidates", lc.candidates, "model", modelID, "err", err)
		return err
	}

	// Pass 1: per-chunk budget enforcement
	var fullText strings.Builder
	var accumulated int
	var usage llm.StreamUsage
	var final *domain.GenerateResponse
	cutoff := int(float64(lc.ss.TokenLimit) * 0.9)
	overrun := false

	for chunk := range chunks {
		usage.Observe(chunk)
		if chunk.Response != nil {
			final = chunk.Response
		}
		if chunk.Err != nil {
			streamFault = chunk.Err
		}
		if chunk.ToolCall != nil && chunk.Text == "" && !chunk.IsFinal {
			// A tool-call fragment (ADR-0139 amendment 2026-11-06) carries no text
			// for a text-lane client; the final chunk's Response has the calls.
			continue
		}
		g.mu.Lock()
		if s, ok := g.sessions[sessionID]; ok {
			g.touchLocked(s)
		}
		g.mu.Unlock()

		fullText.WriteString(chunk.Text)
		accumulated += llm.EstimateTokens(chunk.Text)

		if accumulated >= cutoff && !overrun {
			overrun = true
			chunk.IsFinal = true
			out <- chunk
			break
		}
		if chunk.IsFinal {
			out <- chunk
			break
		}
		out <- chunk
	}

	if overrun {
		// Waiting for a runaway generation to finish would defeat the cut: cancel
		// it, then drain so every layer down to the adapter can close.
		cancel()
		for range chunks {
		}
	} else {
		// Providers often send usage AFTER the first final chunk (OpenAI's usage
		// chunk trails finish_reason). Read the rest — nothing more is forwarded —
		// so the usage is real rather than estimated.
		for chunk := range chunks {
			usage.Observe(chunk)
			if chunk.Response != nil {
				final = chunk.Response
			}
		}
	}

	// Pass 2: post-stream reconciliation. Provider usage when the stream carried
	// it; otherwise the len/4 estimate over prompt AND response, flagged
	// Estimated (ADR-0131 D2).
	var reconciled llm.TokenUsage
	if final != nil && !overrun {
		reconciled = llm.ResponseUsage(lc.prompt, *final)
	} else {
		reconciled = llm.ReconcileTokens(lc.prompt, usage.Chunk(), fullText.String())
	}

	g.mu.Lock()
	if s, ok := g.sessions[sessionID]; ok {
		addLeaseUsageLocked(s, reconciled)
		s.ConsumedTokens = accumulated
		// The Pass-1 cut is the server-authoritative overrun; it used to be a local
		// that died here (T-20261031-taskevent-budgetoverrun-is-written-by-no-kernel).
		if overrun {
			s.BudgetOverrun = true
		}
	}
	g.mu.Unlock()
	lc.meter.RecordSessionTally(lc.sid, lc.ss.Tally, g.leasePurpose(lc.ss), modelID, reconciled)
	return nil
}

// recordCallOutcome keeps the lease's LastFault true to its latest model call
// (ENG-2124): a provider failure sets its class, a success clears it, and an error
// that is not a provider failure (a cancelled turn, a capability answer, a hook
// refusal) leaves it as it was. A no-op for an unknown lease.
func (g *SubstrateLLMGateway) recordCallOutcome(leaseID domain.LeaseID, err error) {
	fault := classifyProviderFault(err)
	if err != nil && fault == domain.ProviderFaultNone {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.sessions[leaseID]; ok {
		s.LastFault = fault
	}
}

// LeaseFault returns the class of the last failed model call under leaseID, or
// ProviderFaultNone when the last call succeeded, none failed, or the lease is
// unknown. The chat lane reads it when a turn fails (TurnService.SetLeaseFaultProbe).
func (g *SubstrateLLMGateway) LeaseFault(leaseID domain.LeaseID) domain.ProviderFault {
	if g == nil || leaseID == "" {
		return domain.ProviderFaultNone
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if s, ok := g.sessions[leaseID]; ok {
		return s.LastFault
	}
	return domain.ProviderFaultNone
}

// ErrToolCallingUnsupported is returned when the model serving a step cannot do
// native tool-calling. It is a CAPABILITY answer, not a failure: the caller is
// expected to fall back to the prompt-encoded action protocol, so it must stay
// distinguishable from a real generation error.
var ErrToolCallingUnsupported = errors.New("tool_calling_unsupported")

// GenerateWithTools runs one managed C1 turn, normally with native tool-calling.
//
// It shares every guard with StreamChunks — CONWIP, lease lookup, the model chain,
// admission — because a tool turn is the same managed LLM call with a different
// response shape. Diverging on any of those would let the tool path escape budget
// control.
func (g *SubstrateLLMGateway) GenerateWithTools(ctx context.Context, sessionID domain.LeaseID, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	return g.GenerateWithToolsObserved(ctx, sessionID, req, nil)
}

// GenerateWithToolsObserved is GenerateWithTools reporting the turn's live deltas
// to observe (ADR-0139 amendment 2026-11-06). The guards, metering and response
// are identical; only the transport to the provider differs — streamed when the
// model source can stream (ObservedStepModels), unary otherwise. observe == nil is
// GenerateWithTools.
func (g *SubstrateLLMGateway) GenerateWithToolsObserved(ctx context.Context, sessionID domain.LeaseID, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (_ domain.GenerateResponse, err error) {
	defer func() { g.recordCallOutcome(sessionID, err) }()
	release, err := g.acquireSlot(ctx)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	defer release()

	lc, err := g.beginLeaseCall(ctx, sessionID, req)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	defer g.trackCall(sessionID)()
	ctx = domain.WithTraceAttributes(ctx, lc.traceAttrs)

	var resp domain.GenerateResponse
	var modelID string
	if om, ok := lc.models.(ObservedStepModels); ok && observe != nil {
		resp, modelID, err = om.GenerateOnObserved(ctx, lc.candidates, req, observe)
	} else {
		resp, modelID, err = lc.models.GenerateOn(ctx, lc.candidates, req)
	}
	if err != nil {
		if len(req.Tools) > 0 && isToolsUnsupported(err) {
			// Capability, not health: the Provider's breaker does not count an
			// unsupported field, so the model stays in service for text.
			return domain.GenerateResponse{}, fmt.Errorf("%w: model %s: %w", ErrToolCallingUnsupported, modelID, err)
		}
		slog.Error("llm_gateway: generation failed",
			"candidates", lc.candidates, "model", modelID, "err", err)
		return domain.GenerateResponse{}, err
	}

	// The provider's usage when the response carried it, else the flagged len/4
	// estimate over the conversation and the turn — prose plus serialized
	// arguments, since a turn that is ALL tool calls has no prose.
	turnText := llm.TurnText(resp.Turn())
	reconciled := llm.ResponseUsage(lc.prompt, resp)
	g.mu.Lock()
	if st, ok := g.sessions[sessionID]; ok {
		g.touchLocked(st)
		st.ConsumedTokens += llm.EstimateTokens(turnText)
		addLeaseUsageLocked(st, reconciled)
	}
	g.mu.Unlock()
	lc.meter.RecordSessionTally(lc.sid, lc.ss.Tally, g.leasePurpose(lc.ss), modelID, reconciled)
	return resp, nil
}

// isToolsUnsupported reports whether err is the C1 refusal of the tools field
// (domain.UnsupportedField(domain.FieldTools, ...)).
func isToolsUnsupported(err error) bool {
	return errors.Is(err, domain.ErrUnsupported) &&
		strings.Contains(err.Error(), domain.ErrUnsupported.Error()+": "+domain.FieldTools)
}
