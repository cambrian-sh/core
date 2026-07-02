package network

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
)

type modelHealthState int

const (
	healthHealthy     modelHealthState = iota
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
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (string, error)
	Complete(ctx context.Context, sessionID string) (llm.TokenUsage, error)
	EvictExpired()
	StreamChunks(ctx context.Context, sessionID string, prompt string, opts domain.GenerateOptions, out chan<- domain.StreamChunk) error
}

// SubstrateLLMGateway implements LLMGateway.
type SubstrateLLMGateway struct {
	mu             sync.RWMutex
	sessions       map[string]*domain.SessionState
	semaphore      chan struct{}
	healthCache    map[string]*modelHealthEntry
	modelClients   map[string]domain.LLMStreamer // model agent ID → streaming client
	cfg            config.ExecutionConfig
	clientFactory  func(agentID string) (domain.LLMStreamer, error)
	Observer       domain.TelemetryObserver // ADR-0019: may be nil (no op)
	// defaultModelID is the streaming model used when a step's StepAllocation has
	// no model winner (the auction returned no TraitModel candidate). It is the
	// streaming-path analogue of the broker's default generator that serves the
	// organs, so an agent step still runs instead of failing. Empty ⇒ no fallback.
	defaultModelID string
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
		sessions:    make(map[string]*domain.SessionState),
		semaphore:   make(chan struct{}, cfg.LLMGatewayMaxConcurrency),
		healthCache: make(map[string]*modelHealthEntry),
		modelClients: make(map[string]domain.LLMStreamer),
		cfg:         cfg,
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

// Acquire creates a new session for a step, allocating a session token UUID.
func (g *SubstrateLLMGateway) Acquire(_ context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (string, error) {
	sessionID := fmt.Sprintf("sess-%d-%d", time.Now().UnixNano(), rand.Int63()%10000)
	now := time.Now()
	ttl := time.Duration(float64(estimatedDuration) * g.cfg.SessionTokenTTLMultiplier)
	if ttl < minSessionTTL {
		ttl = minSessionTTL
	}
	g.mu.Lock()
	g.sessions[sessionID] = &domain.SessionState{
		StepAllocation: sa,
		TokenLimit:     tokenLimit,
		ExpiresAt:      now.Add(ttl),
		LastActivityAt: now,
	}
	g.mu.Unlock()
	return sessionID, nil
}

// Complete reads the final reconciled token usage and removes the session.
func (g *SubstrateLLMGateway) Complete(_ context.Context, sessionID string) (llm.TokenUsage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ss, ok := g.sessions[sessionID]
	if !ok {
		return llm.TokenUsage{}, fmt.Errorf("session not found: %s", sessionID)
	}
	usage := llm.TokenUsage{
		TotalTokens:      ss.ActualTokensUsed,
		CompletionTokens: ss.ActualTokensUsed,
	}
	delete(g.sessions, sessionID)
	return usage, nil
}

// EvictExpired removes all expired session state entries.
func (g *SubstrateLLMGateway) EvictExpired() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, ss := range g.sessions {
		if time.Now().After(ss.ExpiresAt) {
			slog.Warn("session_token_leaked", "session_id", id, "agent_id", ss.StepAllocation.Winner.ID)
			delete(g.sessions, id)
		}
	}
}

// AddSession adds a pre-existing session state (for testing/setup).
func (g *SubstrateLLMGateway) AddSession(id string, ss *domain.SessionState) {
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

// GetSessionState returns a copy of the session state for the given token ID,
// or nil if not found. Used by GenerateViaModelStream for trace attribution.
func (g *SubstrateLLMGateway) GetSessionState(id string) *domain.SessionState {
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
func (g *SubstrateLLMGateway) StreamChunks(ctx context.Context, sessionID string, prompt string, opts domain.GenerateOptions, out chan<- domain.StreamChunk) error {
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
	cutoff := int(float64(ss.TokenLimit) * 0.9)
	overrun := false

	for chunk := range chunks {
		// Keepalive TTL refresh
		g.mu.Lock()
		if s, ok := g.sessions[sessionID]; ok {
			ttl := time.Duration(float64(time.Minute) * g.cfg.SessionTokenTTLMultiplier)
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

	// Pass 2: post-stream reconciliation
	reconciled := llm.ReconcileTokens(accumulated, domain.StreamChunk{UsageTotalTokens: accumulated}, fullText)

	// If the model returned usage in final chunks, try to get exact count
	// (already handled through the chunks loop above)
	_ = reconciled

	g.mu.Lock()
	if s, ok := g.sessions[sessionID]; ok {
		s.ActualTokensUsed = reconciled.TotalTokens
		s.ConsumedTokens = accumulated
	}
	g.mu.Unlock()

	// Update health cache on success
	g.markHealthy(modelID)

	return nil
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
