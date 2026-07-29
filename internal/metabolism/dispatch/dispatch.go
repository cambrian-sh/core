// Package dispatch implements capability-typed dispatch (ADR-0100): agent
// selection as a PURE FUNCTION over data the kernel already owns — zero RPCs,
// zero LLM calls, zero speculative process boots.
//
// It replaces the auction, which solicited a bid from every candidate (booting
// each one to do it) and received a constant from a hand-written keyword table.
// The three decisions the auction conflated are separated here:
//
//	Eligibility  — Gatekeeper L1: required_capabilities ⊆ manifest.Capabilities
//	Ranking      — Gatekeeper L3 merit (per-capability, ADR-0069/0076)
//	Recovery     — cascade on verifier failure (ADR-0100 P5; not in P0)
//
// Only the WINNER is booted. FindCandidates already returns a merit-ranked
// slate, so dispatch is largely the auction with the bid round removed.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// AgentCaller invokes one agent and returns its response.
//
// It is an interface rather than an owned implementation because the dial /
// boot / connection-pool machinery still lives on the Auctioneer while the
// auction remains available as the A/B arm (ADR-0100 P0). Sharing it avoids a
// second gRPC connection pool. When the auction is deleted (P3) that machinery
// moves into this package and the interface is satisfied in-package.
type AgentCaller interface {
	CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error)
}

// ManifestReader reads a registered agent's manifest.
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// BootCounter reports the process-lifetime agent spawn tally. A delta across a
// selection measures what that decision cost in cold starts (ADR-0100 P2).
// Satisfied by *agentmgr.AgentManager; optional and nil-safe.
type BootCounter interface{ BootCount() uint64 }

// ProfileReader reads an agent's performance profile. Optional: a nil reader
// disables the cheapest-competent branch and every step takes merit-argmax.
type ProfileReader interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// Selection reasons, recorded on the emitted event so a routing decision can be
// explained after the fact without re-deriving it.
const (
	ReasonArgmaxMerit       = "argmax-merit"
	ReasonCheapestCompetent = "cheapest-competent"
	ReasonExploration       = "exploration"
	ReasonSoleCandidate     = "sole-candidate"
)

// Dispatcher selects and invokes the executor for one step. It satisfies
// domain.Auctioneer so it is a drop-in at the DAG call site.
type Dispatcher struct {
	Gatekeeper domain.Gatekeeper
	Caller     AgentCaller
	Manifests  ManifestReader

	// Profiles supplies latency/cost for the cheapest-competent branch. nil ⇒
	// always argmax (cold-start safe).
	Profiles ProfileReader

	// Boots measures agent cold starts attributable to a selection (ADR-0100 P2).
	// nil ⇒ the metric is reported as 0.
	Boots BootCounter

	ExecCfg  config.ExecutionConfig
	EventBus domain.EventBus          // may be nil
	Observer domain.TelemetryObserver // may be nil

	// ExplorationRate is the probability of picking a random eligible candidate
	// instead of the policy winner. Carried over from the auction (ADR-0100 D9:
	// exploration attaches to a ranking as well as it did to a market).
	ExplorationRate float64

	// Rand is injectable so tests are deterministic. nil ⇒ package rand.
	Rand *rand.Rand
}

func (d *Dispatcher) randFloat() float64 {
	if d.Rand != nil {
		return d.Rand.Float64()
	}
	return rand.Float64()
}

func (d *Dispatcher) randIntn(n int) int {
	if d.Rand != nil {
		return d.Rand.Intn(n)
	}
	return rand.Intn(n)
}

func (d *Dispatcher) emit(ev domain.AuctionEventPayload) {
	if d.EventBus == nil {
		return
	}
	_ = d.EventBus.Publish(ev)
}

// Execute runs the full dispatch pipeline: Gatekeeper → select → CallAgent.
func (d *Dispatcher) Execute(ctx context.Context, task *domain.AuctionTask, in *domain.Handoff) (*domain.AuctionResult, error) {
	// ADR-0100 P2 instrumentation: the selection decision only — the winner's
	// execution is deliberately outside the window.
	selStart := time.Now()
	bootsBefore := uint64(0)
	if d.Boots != nil {
		bootsBefore = d.Boots.BootCount()
	}
	selectionCost := func() (int32, int32) {
		ms := int32(time.Since(selStart).Milliseconds())
		boots := int32(0)
		if d.Boots != nil {
			boots = int32(d.Boots.BootCount() - bootsBefore)
		}
		return ms, boots
	}

	scored, err := d.Gatekeeper.FindCandidates(ctx, task)
	if err != nil {
		// D5 rung 4: the requirements named something the fleet cannot satisfy and
		// no generalist exists. Surface it on the feed as well as to the caller —
		// this is the failure mode that used to die as a generic "no candidates".
		var noMatch *domain.NoCapabilityMatchError
		if errors.As(err, &noMatch) {
			d.emit(domain.AuctionEventPayload{
				TaskID:   task.ID,
				TaskDesc: task.Description,
				Status:   "failed",
				ErrorMsg: noMatch.Error(),
				Funnel:   task.Funnel,
			})
			if d.Observer != nil {
				d.Observer.OnAuctionNoWinner(task.ID)
			}
			return nil, err
		}
		return nil, fmt.Errorf("gatekeeper failed: %w", err)
	}

	d.emit(domain.AuctionEventPayload{
		TaskID:   task.ID,
		TaskDesc: task.Description,
		Status:   "started",
	})

	if len(scored) == 0 {
		// No eligible executor. Under the auction this surfaced as "no valid
		// proposals"; here it is named for what it is. The D5 resolution ladder
		// (alias map → generalist tier) lands in P1 — until then this is a hard,
		// loud failure rather than a silent misroute.
		d.emit(domain.AuctionEventPayload{
			TaskID:   task.ID,
			TaskDesc: task.Description,
			Status:   "failed",
			ErrorMsg: "no eligible candidates: no registered agent declares the step's required capabilities",
			Funnel:   task.Funnel,
		})
		if d.Observer != nil {
			d.Observer.OnAuctionNoWinner(task.ID)
		}
		return nil, fmt.Errorf("dispatch: no eligible candidates for task %s (required capabilities: %v)",
			task.ID, task.RequiredCapabilities)
	}

	winner, reason := d.selectWinner(ctx, task, scored)

	// The candidate slate is reported on the same event the auction used, so the
	// orchestration suite scores routing accuracy identically across both arms.
	// Confidence carries the MERIT score, not a bid — labelled in Rationale so
	// the two arms are never confused when the numbers are compared.
	bids := make([]domain.BidEntry, 0, len(scored))
	for _, sc := range scored {
		// IsTool is deliberately left false: it recorded the static-bidder path
		// (deprecated with TraitTool), and dispatch solicits no bids at all.
		bids = append(bids, domain.BidEntry{
			AgentID:    sc.Agent.ID,
			Confidence: float32(sc.Score),
			Rationale:  "merit-rank (dispatch: no bid solicited)",
		})
	}

	selMs, selBoots := selectionCost()
	d.emit(domain.AuctionEventPayload{
		TaskID:             task.ID,
		TaskDesc:           task.Description,
		Status:             "completed",
		WinnerID:           winner.Agent.ID,
		Bids:               bids,
		WinnerMargin:       meritMargin(scored, winner.Agent.ID),
		Funnel:             task.Funnel,
		SelectionLatencyMs: selMs,
		SelectionBoots:     selBoots,
	})

	slog.Debug("dispatch: winner selected",
		"task", task.ID, "winner", winner.Agent.ID,
		"reason", reason, "candidates", len(scored), "merit", winner.Score)

	runnerUps := make([]domain.ScoredCandidate, 0, len(scored))
	for _, sc := range scored {
		if sc.Agent.ID != winner.Agent.ID {
			runnerUps = append(runnerUps, sc)
		}
	}

	// ADR-0023 Fix 2: inject the winning capability so the SDK's _dispatch_execute
	// routes to the right handler without text-matching fallback.
	if in.Context == nil {
		in.Context = make(map[string]string)
	}
	if d.Manifests != nil {
		if m, mErr := d.Manifests.GetManifest(ctx, winner.Agent.ID); mErr == nil && m != nil && len(m.Tools) > 0 {
			in.Context["_capability"] = m.Tools[0]
		}
	}
	in.Context["_selection_reason"] = reason

	// NOTE (ADR-0100 D9): the auction's requirement sub-negotiation fired on
	// bestProposal.Requirements. With no bids there are no requirements, so that
	// recursive path is inert here by construction. Re-homing it as a step-level
	// declaration is tracked in the ADR; it must not be dropped by omission.

	resp, err := d.Caller.CallAgent(ctx, winner.Agent.ID, in, "")
	if err != nil {
		in.Context["_winning_agent_id"] = winner.Agent.ID
		return &domain.AuctionResult{Confidence: winner.Score, RunnerUps: runnerUps}, err
	}

	return &domain.AuctionResult{
		Handoff:        resp,
		Confidence:     winner.Score,
		RunnerUps:      runnerUps,
		StepAllocation: d.selectModelCandidates(ctx, winner.Agent.ID),
	}, nil
}

// selectWinner implements the ADR-0100 D4 per-step dispatch policy.
//
// Pure argmax-on-merit has a rich-get-richer failure: the top-ranked agent
// accumulates all the evidence, every other agent stays cold, and the ranking
// freezes on early noise. Pure cheapest-first pays a re-execution whenever it
// guesses wrong. The step itself already says which regime it is in — a cheap
// step whose result will be verified can afford to try the cheap agent, because
// the checkpoint catches a bad answer and escalation is affordable.
func (d *Dispatcher) selectWinner(ctx context.Context, task *domain.AuctionTask, scored []domain.ScoredCandidate) (domain.ScoredCandidate, string) {
	if len(scored) == 1 {
		return scored[0], ReasonSoleCandidate
	}

	if d.ExplorationRate > 0 && d.randFloat() < d.ExplorationRate {
		pick := d.randIntn(len(scored))
		return scored[pick], ReasonExploration
	}

	if d.cheapAndVerified(task) {
		if w, ok := d.cheapestCompetent(ctx, scored); ok {
			return w, ReasonCheapestCompetent
		}
	}

	// FindCandidates returns merit-ranked, highest first.
	return scored[0], ReasonArgmaxMerit
}

// cheapAndVerified reports whether the step is in the regime where trying the
// cheapest competent agent is the right bet: its output will be checked, and its
// energy budget marks it as cheap. A step with no declared budget (MaxEnergy 0)
// is NOT treated as cheap — absence of a budget is not a claim of cheapness.
func (d *Dispatcher) cheapAndVerified(task *domain.AuctionTask) bool {
	if !task.CheckpointAfter {
		return false
	}
	maxE := d.ExecCfg.DispatchCheapEnergyMax
	if maxE <= 0 {
		return false
	}
	return task.MaxEnergy > 0 && task.MaxEnergy <= maxE
}

// cheapestCompetent picks the lowest-cost candidate whose merit clears the floor.
// Returns ok=false when no profile data is available or nobody clears the floor,
// so the caller falls back to argmax.
func (d *Dispatcher) cheapestCompetent(ctx context.Context, scored []domain.ScoredCandidate) (domain.ScoredCandidate, bool) {
	if d.Profiles == nil {
		return domain.ScoredCandidate{}, false
	}
	floor := d.ExecCfg.DispatchMeritFloor

	var best domain.ScoredCandidate
	bestCost := 0.0
	found := false

	for _, sc := range scored {
		if sc.Score < floor {
			continue
		}
		p, err := d.Profiles.GetProfile(ctx, sc.Agent.ID, sc.Agent.SourceHash)
		if err != nil || p == nil {
			continue // no evidence of cost ⇒ not a basis for preferring it
		}
		cost := agentCost(p)
		if !found || cost < bestCost {
			best, bestCost, found = sc, cost, true
		}
	}
	return best, found
}

// agentCost is the cheapness ordering: money first when the profile records it,
// latency otherwise. Both are observed, never self-reported.
func agentCost(p *domain.AgentProfile) float64 {
	if p.ModelMetrics != nil && p.ModelMetrics.AvgCostPerTask > 0 {
		return p.ModelMetrics.AvgCostPerTask
	}
	return float64(p.NetworkLatencyMedianMs + p.ComputationLatencyMedianMs)
}

// meritMargin is the winner's merit minus the best non-winner's — the dispatch
// analogue of the auction's WinnerMargin. A near-zero margin flags a coin-flip.
func meritMargin(scored []domain.ScoredCandidate, winnerID string) float32 {
	var winner float64
	runnerUp := 0.0
	haveWinner, haveRunnerUp := false, false
	for _, sc := range scored {
		if sc.Agent.ID == winnerID && !haveWinner {
			winner, haveWinner = sc.Score, true
			continue
		}
		if !haveRunnerUp || sc.Score > runnerUp {
			runnerUp, haveRunnerUp = sc.Score, true
		}
	}
	if !haveWinner || !haveRunnerUp {
		return 0
	}
	return float32(winner - runnerUp)
}

// selectModelCandidates runs ADR-0018 TraitModel sub-selection for the winning
// agent. Behaviour-identical to the Auctioneer's, so model allocation is
// unchanged across the two arms. Returns nil when no TraitModel agents exist.
func (d *Dispatcher) selectModelCandidates(ctx context.Context, winnerAgentID string) *domain.StepAllocation {
	if d.Manifests == nil {
		return nil
	}
	manifest, err := d.Manifests.GetManifest(ctx, winnerAgentID)
	if err != nil || manifest == nil {
		return nil
	}
	models, err := d.Gatekeeper.FindModelCandidates(ctx, manifest.RequiredModelCapabilities)
	if err != nil || len(models) == 0 {
		return nil
	}
	sa := &domain.StepAllocation{Winner: models[0].Agent}
	if len(models) >= 2 {
		sa.Fallbacks[0] = models[1].Agent
	}
	if len(models) >= 3 {
		sa.Fallbacks[1] = models[2].Agent
	}
	return sa
}

// CallAgent satisfies domain.Auctioneer by delegating to the shared caller.
func (d *Dispatcher) CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
	return d.Caller.CallAgent(ctx, agentID, handoff, excludeInstanceID)
}
