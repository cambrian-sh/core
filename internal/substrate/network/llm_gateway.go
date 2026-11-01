package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

type modelHealthState int

const (
	healthHealthy modelHealthState = iota
	healthUnhealthy
	healthRateLimited
)

type modelHealthEntry struct {
	state     modelHealthState
	expiresAt time.Time
	reason    string // last failure cause (e.g. "openai: HTTP 401: ...") — surfaced in the degraded error
}

// LLMGateway is the central control point for ADR-0018 managed LLM calls.
// It owns the session store, CONWIP semaphore, and health cache circuit breaker.
type LLMGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error)
	Complete(ctx context.Context, leaseID domain.LeaseID) (llm.TokenUsage, error)
	EvictExpired()
	StreamChunks(ctx context.Context, leaseID domain.LeaseID, prompt string, opts domain.GenerateOptions, out chan<- domain.StreamChunk) error
	// GenerateWithTools runs ONE managed generation turn with native tool-calling
	// (ADR-0097 Phase B). Returns ErrToolCallingUnsupported when the allocated model
	// cannot do it, which callers translate into the prompt-encoded fallback rather
	// than a failure.
	GenerateWithTools(ctx context.Context, leaseID domain.LeaseID, messages []domain.ModelMessage, opts domain.GenerateOptions, tools []domain.ToolDefinition) (domain.ModelTurn, error)
}

// SubstrateLLMGateway implements LLMGateway.
type SubstrateLLMGateway struct {
	mu            sync.RWMutex
	sessions      map[domain.LeaseID]*domain.BudgetLeaseState
	semaphore     chan struct{}
	healthCache   map[string]*modelHealthEntry
	modelClients  map[string]domain.LLMStreamer // model agent ID → streaming client
	cfg           config.ExecutionConfig
	clientFactory func(agentID string) (domain.LLMStreamer, error)
	Observer      domain.TelemetryObserver // ADR-0019: may be nil (no op)
	// defaultModelID is the streaming model used when a step's StepAllocation has
	// no model winner (the auction returned no TraitModel candidate). It is the
	// streaming-path analogue of the broker's default generator that serves the
	// organs, so an agent step still runs instead of failing. Empty ⇒ no fallback.
	defaultModelID string

	// meter is the ADR-0131 D1 spend ledger — the SAME object the Provider meters
	// the organs with, so agent streams and organ calls add up in one place.
	// Nil = no session ceiling and no spend record (tests, maintenance tools).
	meter atomic.Pointer[llm.SpendMeter]

	// onComplete runs after a lease is completed and removed. It is the ONE place
	// "lease finished" side effects hang, so a new lease site (the retrieval
	// dispatcher was the third) cannot forget them: today it clears the ADR-0046
	// D6 run-scoped skill grants keyed by the lease
	// (T-20261101-retrieval-dispatch-lease-retrieval-dispatch-go).
	onComplete atomic.Pointer[func(domain.LeaseID)]
}

// SetSpendMeter attaches the ADR-0131 spend ledger. Every agent stream and tool
// turn is then admitted against the session ceiling and booked there.
func (g *SubstrateLLMGateway) SetSpendMeter(m *llm.SpendMeter) { g.meter.Store(m) }

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

// NewLLMGateway creates a wired SubstrateLLMGateway.
func NewLLMGateway(cfg config.ExecutionConfig) *SubstrateLLMGateway {
	gw := &SubstrateLLMGateway{
		sessions:     make(map[domain.LeaseID]*domain.BudgetLeaseState),
		semaphore:    make(chan struct{}, cfg.LLM.LLMGatewayMaxConcurrency),
		healthCache:  make(map[string]*modelHealthEntry),
		modelClients: make(map[string]domain.LLMStreamer),
		cfg:          cfg,
	}
	_ = gw // caller injects clientFactory and modelClients after construction
	return gw
}

// SetClientFactory configures how model streaming clients are resolved.
func (g *SubstrateLLMGateway) SetClientFactory(f func(agentID string) (domain.LLMStreamer, error)) {
	g.clientFactory = f
}

// RegisterModelClient pre-registers a streaming client for a model agent ID.
func (g *SubstrateLLMGateway) RegisterModelClient(agentID string, client domain.LLMStreamer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.modelClients[agentID] = client
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

// Acquire creates a new session for a step, allocating a session token UUID.
//
// A lease acquired under a domain.WithBackgroundSpend ctx is a background lease
// (ADR-0131, owner decision 2026-11-01): its calls are booked to the session it
// is bound to, never refused at that session's ceiling. The marker is read here,
// from the kernel-side ctx that mints the lease, so no agent can claim it.
func (g *SubstrateLLMGateway) Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error) {
	sessionID := domain.LeaseID(fmt.Sprintf("lease-%d-%d", time.Now().UnixNano(), rand.Int63()%10000))
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

// ResolveLease returns the identity bound to leaseID (domain.LeaseResolver).
//
// The bool reports whether the lease is KNOWN, not whether it carries a binding — a live
// but unbound lease returns (zero, true). Transports rely on that distinction: "known
// lease" means the caller is an agent and its header must not be read as a session ID,
// even when no session was bound.
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
	defer g.mu.Unlock()
	if ss, ok := g.sessions[leaseID]; ok {
		addLeaseUsageLocked(ss, u)
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
		if time.Now().After(ss.ExpiresAt) {
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

// StreamChunks opens a managed stream to the allocated TraitModel.
// It applies CONWIP, health cache routing, Pass 1 budget enforcement,
// and Pass 2 reconciliation.
func (g *SubstrateLLMGateway) StreamChunks(ctx context.Context, sessionID domain.LeaseID, prompt string, opts domain.GenerateOptions, out chan<- domain.StreamChunk) error {
	start := time.Now()
	select {
	case g.semaphore <- struct{}{}:
		defer func() { <-g.semaphore }()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("gateway_overflow")
	}
	if g.Observer != nil {
		g.Observer.OnConwipWait(time.Since(start).Milliseconds())
	}

	g.mu.RLock()
	ss, ok := g.sessions[sessionID]
	g.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Build the candidate chain from StepAllocation, skipping empty ids (an empty
	// Winner.ID means the auction allocated no model to this step).
	modelIDs := make([]string, 0, 4)
	if ss.StepAllocation.Winner.ID != "" {
		modelIDs = append(modelIDs, ss.StepAllocation.Winner.ID)
	}
	if ss.StepAllocation.Fallbacks[0].ID != "" {
		modelIDs = append(modelIDs, ss.StepAllocation.Fallbacks[0].ID)
	}
	if ss.StepAllocation.Fallbacks[1].ID != "" {
		modelIDs = append(modelIDs, ss.StepAllocation.Fallbacks[1].ID)
	}
	// Resilience: when no model was allocated (empty StepAllocation — e.g. no
	// TraitModel agent is registered or matched the step), fall back to the
	// configured default model so the agent still generates, instead of failing
	// the step. This is the streaming-path analogue of the broker's default
	// generator that already serves the organs (planner/memory/verifier).
	g.mu.RLock()
	def := g.defaultModelID
	g.mu.RUnlock()
	if len(modelIDs) == 0 && def != "" {
		slog.Warn("llm_gateway: step has no allocated model; using configured default model",
			"default_model", def, "session", sessionID)
		modelIDs = append(modelIDs, def)
	}
	if len(modelIDs) == 0 {
		return fmt.Errorf("no model allocated for session %s and no default model configured", sessionID)
	}

	// ADR-0131 D3: the session ceiling is checked before any model is touched.
	meter := g.meter.Load()
	sid := leaseSession(ctx, ss)
	if err := admitLease(meter, ss, sid, prompt); err != nil {
		return err
	}

	// Find first healthy model
	modelID, err := g.selectHealthyModel(modelIDs)
	if err != nil {
		return err
	}

	streamer, err := g.getStreamer(modelID)
	if err != nil {
		slog.Error("llm_gateway: no streaming client for model (check that the generator is configured and its provider is supported)",
			"model", modelID, "err", err)
		return err
	}

	// Open stream from the model
	chunks, err := streamer.GenerateStream(ctx, prompt)
	if err != nil {
		// Surface the REAL cause (e.g. "openai: HTTP 401", connection refused, bad
		// endpoint). Without this the only thing the agent ever sees is the generic
		// "model_unavailable: all candidates degraded" raised by the next call's
		// fast-fail within the cooldown window — the actual failure is invisible.
		slog.Error("llm_gateway: model stream failed; marking degraded for cooldown",
			"model", modelID, "err", err)
		g.markUnhealthy(modelID, err.Error())
		return err
	}

	// Pass 1: per-chunk budget enforcement
	var fullText string
	var accumulated int
	var usage llm.StreamUsage
	cutoff := int(float64(ss.TokenLimit) * 0.9)
	overrun := false

	for chunk := range chunks {
		usage.Observe(chunk)
		// Keepalive TTL refresh
		g.mu.Lock()
		if s, ok := g.sessions[sessionID]; ok {
			ttl := time.Duration(float64(time.Minute) * g.cfg.Session.SessionTokenTTLMultiplier)
			s.ExpiresAt = time.Now().Add(ttl)
			s.LastActivityAt = time.Now()
		}
		g.mu.Unlock()

		fullText += chunk.Text
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

	// Providers often send usage AFTER the first final chunk (OpenAI's usage chunk
	// trails finish_reason). On a natural end, read the rest of the stream — the
	// agent has its answer already, nothing more is forwarded — so the usage is
	// real rather than estimated. Not after a Pass-1 cut: waiting for a runaway
	// generation to finish would defeat the cut.
	if !overrun {
		for chunk := range chunks {
			usage.Observe(chunk)
		}
	}

	// Pass 2: post-stream reconciliation. Provider usage when the stream carried
	// it; otherwise the len/4 estimate over prompt AND response, flagged
	// Estimated (ADR-0131 D2). This used to compute a value and discard it.
	reconciled := llm.ReconcileTokens(prompt, usage.Chunk(), fullText)

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
	meter.RecordSession(sid, string(domain.PurposeAgentStep), modelID, reconciled)

	// Update health cache on success
	g.markHealthy(modelID)

	return nil
}

// ErrToolCallingUnsupported is returned when the model allocated to a step cannot do
// native tool-calling. It is a CAPABILITY answer, not a failure: the caller is
// expected to fall back to the prompt-encoded action protocol, so it must stay
// distinguishable from a real generation error.
var ErrToolCallingUnsupported = errors.New("tool_calling_unsupported")

// GenerateWithTools runs one managed generation turn with native tool-calling.
//
// It reuses the same guards as StreamChunks — CONWIP, session lookup, allocation
// chain with default-model fallback, health selection — because a tool-calling turn
// is the same managed LLM call, just with a different response shape. Diverging on
// any of those would mean the tool path quietly escapes budget or health control.
func (g *SubstrateLLMGateway) GenerateWithTools(
	ctx context.Context, sessionID domain.LeaseID, messages []domain.ModelMessage,
	opts domain.GenerateOptions, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	var zero domain.ModelTurn

	select {
	case g.semaphore <- struct{}{}:
		defer func() { <-g.semaphore }()
	case <-ctx.Done():
		return zero, ctx.Err()
	default:
		return zero, fmt.Errorf("gateway_overflow")
	}

	g.mu.RLock()
	ss, ok := g.sessions[sessionID]
	g.mu.RUnlock()
	if !ok {
		return zero, fmt.Errorf("session not found: %s", sessionID)
	}

	modelIDs := make([]string, 0, 4)
	if ss.StepAllocation.Winner.ID != "" {
		modelIDs = append(modelIDs, ss.StepAllocation.Winner.ID)
	}
	for _, fb := range ss.StepAllocation.Fallbacks {
		if fb.ID != "" {
			modelIDs = append(modelIDs, fb.ID)
		}
	}
	g.mu.RLock()
	def := g.defaultModelID
	g.mu.RUnlock()
	if len(modelIDs) == 0 && def != "" {
		modelIDs = append(modelIDs, def)
	}
	if len(modelIDs) == 0 {
		return zero, fmt.Errorf("no model allocated for session %s and no default model configured", sessionID)
	}

	meter := g.meter.Load()
	sid := leaseSession(ctx, ss)
	prompt := llm.MessagesText(messages)
	if err := admitLease(meter, ss, sid, prompt); err != nil {
		return zero, err
	}

	modelID, err := g.selectHealthyModel(modelIDs)
	if err != nil {
		return zero, err
	}

	client, err := g.getStreamer(modelID)
	if err != nil {
		return zero, err
	}
	tg, ok := client.(domain.ToolCallingGenerator)
	if !ok || !domain.SupportsToolCalling(client) {
		// Capability, not health: do NOT mark the model degraded. A model that
		// cannot do tool-calling is perfectly healthy for text generation, and
		// poisoning the health cache here would take it out of service entirely.
		return zero, fmt.Errorf("%w: model %s", ErrToolCallingUnsupported, modelID)
	}

	cctx, capture := llm.WithUsageCapture(ctx)
	turn, err := tg.GenerateWithTools(cctx, messages, tools)
	if err != nil {
		slog.Error("llm_gateway: tool-calling generation failed; marking degraded for cooldown",
			"model", modelID, "err", err)
		g.markUnhealthy(modelID, err.Error())
		return zero, err
	}

	// Budget accounting on the same basis as the streaming path: the provider's
	// usage when the response carried it, else the flagged len/4 estimate over the
	// conversation and the turn — prose plus serialized arguments, since a turn
	// that is ALL tool calls has no prose and would otherwise bill as zero.
	turnText := llm.TurnText(turn)
	reconciled, reported := capture.Usage()
	if !reported {
		reconciled = llm.EstimateUsage(prompt, turnText)
	}
	g.mu.Lock()
	if st, ok := g.sessions[sessionID]; ok {
		ttl := time.Duration(float64(time.Minute) * g.cfg.Session.SessionTokenTTLMultiplier)
		st.ExpiresAt = time.Now().Add(ttl)
		st.LastActivityAt = time.Now()
		st.ConsumedTokens += llm.EstimateTokens(turnText)
		addLeaseUsageLocked(st, reconciled)
	}
	g.mu.Unlock()
	meter.RecordSession(sid, string(domain.PurposeAgentStep), modelID, reconciled)

	g.markHealthy(modelID)
	return turn, nil
}

func (g *SubstrateLLMGateway) selectHealthyModel(modelIDs []string) (string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	now := time.Now()
	reasons := make([]string, 0, len(modelIDs))
	nonEmpty := 0
	for _, mid := range modelIDs {
		if mid == "" {
			continue
		}
		nonEmpty++
		entry, ok := g.healthCache[mid]
		if !ok || entry.state == healthHealthy {
			return mid, nil
		}
		if now.After(entry.expiresAt) {
			delete(g.healthCache, mid)
			return mid, nil
		}
		r := entry.reason
		if r == "" {
			r = "recent failure"
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", mid, r))
	}
	if nonEmpty == 0 {
		// No model was allocated to this step (StepAllocation.Winner.ID is empty,
		// and no fallbacks). This is an ALLOCATION problem — the auction returned no
		// model candidate — not a health/availability one, so the cooldown cache is
		// irrelevant. Distinguished from real degradation so the error stops blaming
		// model health for what is an empty allocation.
		slog.Error("llm_gateway: no model allocated to this step — empty StepAllocation winner. The auction produced no model candidate: check the agent manifest's RequiredModelCapabilities and that a matching TraitModel is registered in the auction registry.")
		return "", fmt.Errorf("no_model_allocated: step has no model winner (empty StepAllocation)")
	}
	slog.Warn("llm_gateway: all model candidates are in health-cache cooldown",
		"candidates", modelIDs, "reasons", reasons)
	return "", fmt.Errorf("model_unavailable: all %d candidates degraded (%s)", nonEmpty, strings.Join(reasons, "; "))
}

func (g *SubstrateLLMGateway) getStreamer(modelID string) (domain.LLMStreamer, error) {
	g.mu.RLock()
	s, ok := g.modelClients[modelID]
	g.mu.RUnlock()
	if ok {
		return s, nil
	}
	if g.clientFactory != nil {
		return g.clientFactory(modelID)
	}
	return nil, fmt.Errorf("no streaming client for model %s", modelID)
}

func (g *SubstrateLLMGateway) markHealthy(modelID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.healthCache, modelID)
}

func (g *SubstrateLLMGateway) markUnhealthy(modelID, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthCache[modelID] = &modelHealthEntry{
		state:     healthUnhealthy,
		expiresAt: time.Now().Add(30 * time.Second),
		reason:    reason,
	}
}

// MarkUnhealthy exposes health cache updates for testing.
func (g *SubstrateLLMGateway) MarkUnhealthy(modelID string) {
	g.markUnhealthy(modelID, "forced unhealthy (test)")
}

// HealthState returns the current state of a model in the health cache (for testing).
func (g *SubstrateLLMGateway) HealthState(modelID string) modelHealthState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	e, ok := g.healthCache[modelID]
	if !ok {
		return healthHealthy
	}
	if time.Now().After(e.expiresAt) {
		return healthHealthy
	}
	return e.state
}
