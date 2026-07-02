package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
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
	// ScoutModel is the model id Scout reasons with (a cheap/fast model is ideal — discovery
	// is light perception on the hot path). "" ⇒ empty allocation ⇒ the gateway default.
	ScoutModel string
}

// Discover invokes the Scout agent and returns its report merged with deterministic env
// facts. Never errors: a dispatch failure yields an env-only report (still grounds paths).
func (d *AgentScoutDispatcher) Discover(ctx context.Context, userInput string) *domain.DiscoveryReport {
	env := computeScoutEnv() // deterministic OS/path grounding — ALWAYS provided
	report := &domain.DiscoveryReport{Environment: env}
	if d == nil || d.Auctioneer == nil {
		return report
	}
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
	// Deliberately allocate Scout's model via a managed session (mirroring the normal step
	// path, server.go) so its generate calls don't silently ride the gateway default. Empty
	// ScoutModel ⇒ empty allocation ⇒ the gateway default, but now via a metered session.
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
		slog.Debug("scout dispatch: no report; env grounding only", "err", err)
		return report // dispatch failure ⇒ env-only report (degraded but still grounded)
	}
	parseScoutReportInto(report, resp.Payload.Data)
	return report
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
	report.Unobserved = parsed.Unobserved
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
