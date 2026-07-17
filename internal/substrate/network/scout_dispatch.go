package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/discovery"
)

const (
	scoutTokenLimit = 4096
	scoutSessionTTL = 30 * time.Second
)

// AgentScoutDispatcher implements WorldScout (ADR-0051) by invoking the privileged Python
// Scout agent directly via the Auctioneer (no auction), parsing its structured report, and
// merging deterministic environment grounding. The discovery LOOP lives in the Scout agent's
// run_think (find_tools, memory_query, multi-modal read-only tool calls), confined to
// read-only by its `discovery-safe` grant (D6) — this Go side is just dispatch + parse.
type AgentScoutDispatcher struct {
	Auctioneer   domain.Auctioneer
	ScoutAgentID string // default "scout_agent"
	// Gateway acquires Scout's managed LLM session so its generate calls resolve to a
	// DELIBERATE model (ScoutModel) instead of the gateway's default-model fallback — the
	// CallAgent path does not pre-allocate a model the way the normal step path does. nil ⇒
	// no session acquired (Scout falls back to the gateway default, as before).
	Gateway LLMGateway
	// ScoutModel is the fallback model the opt-in LLM tier reasons with (a cheap/fast model
	// is ideal). "" ⇒ empty allocation ⇒ the gateway default.
	ScoutModel string

	// Registry is the deterministic-first discovery engine (ADR-0078 D1/D2) — the PRIMARY
	// path. nil/empty ⇒ no deterministic probes (env-only + optional LLM tier).
	Registry *discovery.Registry
	// LLMTierEnabled turns on the opt-in ADR-0051 run_think scout LAYERED on top of the
	// deterministic probes (ADR-0078 D1). false (default) ⇒ zero LLM on the discovery path.
	LLMTierEnabled bool

	// sessionCache holds the last discovery report per session id (ADR-0078 D5 — session
	// memory). In-process, session-scoped; cleared at session end via ClearSession.
	mu           sync.Mutex
	sessionCache map[string]*domain.DiscoveryReport
}

// Discover produces the pre-plan DiscoveryReport (ADR-0078). Order: deterministic env
// grounding (always) → deterministic probe registry (D1/D2, the primary path, no LLM) →
// the opt-in LLM tier (D1, only when enabled) → session-memory persistence (D5). Never
// errors: with nothing wired it returns an env-only report (still grounds paths).
func (d *AgentScoutDispatcher) Discover(ctx context.Context, userInput string) *domain.DiscoveryReport {
	env := computeScoutEnv() // deterministic OS/path grounding — ALWAYS provided
	report := &domain.DiscoveryReport{Environment: env}
	if d == nil {
		return report
	}

	// Deterministic-first (ADR-0078 D1/D2): structured, trusted, no LLM on the hot path.
	if d.Registry != nil && !d.Registry.Empty() {
		ents, unobserved := d.Registry.Discover(ctx, userInput)
		report.Entities = append(report.Entities, ents...)
		report.Unobserved = append(report.Unobserved, unobserved...)
	}

	// Opt-in LLM tier (ADR-0078 D1): the ADR-0051 run_think scout, layered on top. Only
	// when explicitly enabled AND an auctioneer is wired — off ⇒ zero LLM discovery cost.
	if d.LLMTierEnabled && d.Auctioneer != nil {
		d.runLLMTier(ctx, userInput, report)
	}

	// Session memory (ADR-0078 D5): keep findings for later steps/replans in this session.
	d.persistToSession(ctx, report)
	return report
}

// runLLMTier invokes the opt-in run_think scout and MERGES its findings into the (already
// deterministic) report. Best-effort: any failure leaves the deterministic findings intact.
func (d *AgentScoutDispatcher) runLLMTier(ctx context.Context, userInput string, report *domain.DiscoveryReport) {
	agentID := d.ScoutAgentID
	if agentID == "" {
		agentID = "scout_agent"
	}
	h := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   agentID,
		Payload:   &domain.Payload{Type: "discovery_request", Data: []byte(userInput)},
		Context:   map[string]string{"task_id": "scout-discovery"},
	}
	// Allocate the tier's model via a managed session (mirroring the normal step path) so
	// its generate calls don't silently ride the gateway default. Empty ScoutModel ⇒ the
	// gateway default, but still via a metered session.
	if d.Gateway != nil {
		sa := domain.StepAllocation{}
		if d.ScoutModel != "" {
			sa.Winner = domain.AgentDefinition{ID: d.ScoutModel}
		}
		if tokenID, aerr := d.Gateway.Acquire(ctx, sa, scoutTokenLimit, scoutSessionTTL); aerr == nil && tokenID != "" {
			h.Context["_session_token_id"] = tokenID
			defer func() { _, _ = d.Gateway.Complete(ctx, tokenID) }()
		}
	}
	resp, err := d.Auctioneer.CallAgent(ctx, agentID, h, "")
	if err != nil || resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		slog.Debug("scout LLM tier: no report; deterministic findings only", "err", err)
		return
	}
	parseScoutReportInto(report, resp.Payload.Data)
}

// persistToSession stores the report under the ctx session id (ADR-0078 D5). No session
// id or empty report ⇒ no-op.
func (d *AgentScoutDispatcher) persistToSession(ctx context.Context, report *domain.DiscoveryReport) {
	sid, ok := domain.SessionIDFromContext(ctx)
	if !ok || sid == "" || report.IsEmpty() {
		return
	}
	d.mu.Lock()
	if d.sessionCache == nil {
		d.sessionCache = make(map[string]*domain.DiscoveryReport)
	}
	d.sessionCache[sid] = report
	d.mu.Unlock()
}

// SessionDiscovery returns the last discovery report cached for a session (ADR-0078 D5),
// so later steps/replans can reuse it instead of re-probing.
func (d *AgentScoutDispatcher) SessionDiscovery(sessionID string) (*domain.DiscoveryReport, bool) {
	if d == nil || sessionID == "" {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.sessionCache[sessionID]
	return r, ok
}

// ClearSession drops a session's cached discoveries (ADR-0078 D5 — dropped at session end).
func (d *AgentScoutDispatcher) ClearSession(sessionID string) {
	if d == nil || sessionID == "" {
		return
	}
	d.mu.Lock()
	delete(d.sessionCache, sessionID)
	d.mu.Unlock()
}

// parseScoutReportInto extracts the Scout agent's structured report from its answer (which
// may be wrapped in prose/fences) into report, leaving report.Environment untouched.
func parseScoutReportInto(report *domain.DiscoveryReport, data []byte) {
	m := domain.ExtractJSONObject(string(data))
	if m == "" {
		return
	}
	var parsed struct {
		Entities []struct {
			Kind    string `json:"kind"`
			ID      string `json:"id"`
			Exists  bool   `json:"exists"`
			Summary string `json:"summary"`
		} `json:"entities"`
		Interpretation string   `json:"interpretation"`
		Unobserved     []string `json:"unobserved"`
	}
	if err := json.Unmarshal([]byte(m), &parsed); err != nil {
		return
	}
	report.Interpretation = strings.TrimSpace(parsed.Interpretation)
	report.Unobserved = append(report.Unobserved, parsed.Unobserved...)
	for _, e := range parsed.Entities {
		report.Entities = append(report.Entities, domain.DiscoveredEntity{
			Kind: e.Kind, ID: e.ID, Exists: e.Exists, Summary: strings.TrimSpace(e.Summary),
		})
	}
}

// computeScoutEnv gathers deterministic runtime environment facts (no LLM) so the Planner
// emits correct absolute paths for this host (ADR-0051) — the fix for cross-OS path guesses
// like `~/Desktop` on Windows. Best-effort: missing values are blank, never an error.
func computeScoutEnv() *domain.EnvFacts {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	desktop := ""
	if home != "" {
		desktop = filepath.Join(home, "Desktop")
	}
	return &domain.EnvFacts{OS: runtime.GOOS, Home: home, Desktop: desktop, Cwd: cwd}
}
