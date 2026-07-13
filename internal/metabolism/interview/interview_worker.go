package interview

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"

	"golang.org/x/sync/errgroup"
)

const (
	// DefaultInterviewPoolSize is the number of concurrent goroutines in the pool.
	DefaultInterviewPoolSize = 5

	// DefaultProvisionalScore is the cold-start GatekeeperScore for Provisional agents.
	DefaultProvisionalScore = 0.1

	// DefaultSimilarityThreshold is the minimum cosine similarity for the Interview gate.
	DefaultSimilarityThreshold = 0.5

	// DefaultDecayClampMin is the minimum decay fraction for re-interview Merit inheritance.
	DefaultDecayClampMin = 0.1

	// DefaultDecayClampMax is the maximum decay fraction (full inheritance).
	DefaultDecayClampMax = 1.0
)

// SweepTrigger is the consumer-side interface that signals the CapabilityClusterer
// to run a new sweep. Implemented by CapabilityClusterer; nil disables signalling.
type SweepTrigger interface {
	TriggerSweep()
}

// ManifestReader fetches an agent's manifest by ID.
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// ProfileStore persists and retrieves Agent Profiles in the vector store.
type ProfileStore interface {
	SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
	GetJudicialRecords(ctx context.Context, agentID, sourceHash string, topK int) ([]string, error)
	Save(ctx context.Context, text string, embedding []float32, metadata map[string]interface{}) error
	EmbeddingDistance(a, b []float32) float64
}

// AgentUpdater marks an agent as no longer Provisional in the registry.
type AgentUpdater interface {
	SetProvisional(agentID string, provisional bool) error
}

// CardFetcher retrieves an Agent Card from an A2A endpoint.
type CardFetcher interface {
	FetchCard(ctx context.Context, endpoint string) (*domain.AgentCard, error)
}

// InterviewTaskWriter persists interview-derived TaskEvents for profile seeding.
type InterviewTaskWriter interface {
	WriteTaskEvent(event domain.TaskEvent) error
}

// InterviewWorker is the background goroutine pool that drives the
// Provisional → Active state transition for newly registered or updated agents.
type InterviewWorker struct {
	Registry     ManifestReader
	Embedder     domain.Embedder
	Store        ProfileStore
	Updater      AgentUpdater
	Requester    domain.ProposalRequester // optional — nil disables scenario RPC calls
	CardFetcher  CardFetcher              // optional — required for RuntimeA2A agents
	EventWriter  InterviewTaskWriter      // optional — nil disables interview TaskEvent recording
	SweepTrigger SweepTrigger            // optional — nil disables capability cluster signalling
	// EventBus receives AgentReadyEvent after every Provisional→Active transition.
	// ADR-0023 D6A / ADR-0030. nil-safe.
	EventBus     domain.EventBus
	// Examiner, when set, upgrades the cognitive-agent interview from a self-assessed
	// bid into a graded capability probe: an LLM asks questions, the agent answers
	// them for real, and the answers are graded. The mean grade seeds the agent's
	// cold-start TrustScore/SuccessRate — the routing prior EFE was missing. nil ⇒
	// the legacy bid-only path (backward compatible). ADR-0037 interview grading.
	Examiner     *Examiner
	// BeliefSeeder, when set, also seeds the EFE CapabilityBelief from the interview
	// grade. Optional — the belief store is not yet live (ADR-0037 deferred), so this
	// is a ready seam, nil today.
	BeliefSeeder BeliefSeeder
	PoolSize     int
	queue        chan domain.AgentDefinition
	readyCh      chan string // ADR-0023 D6B: non-blocking; capacity 32; closed on Stop
}

// NewInterviewWorker creates an InterviewWorker with the default pool size.
// The Requester field is nil; scenario RPC calls are disabled.
func NewInterviewWorker(registry ManifestReader, embedder domain.Embedder, store ProfileStore, updater AgentUpdater) *InterviewWorker {
	return &InterviewWorker{
		Registry: registry,
		Embedder: embedder,
		Store:    store,
		Updater:  updater,
		PoolSize: DefaultInterviewPoolSize,
		queue:    make(chan domain.AgentDefinition, 100),
		readyCh:  make(chan string, 32),
	}
}

// ReadyChan returns a channel that receives agent IDs as they complete
// the Provisional→Active transition. The send is non-blocking: if nobody
// is reading, events are dropped (not buffered indefinitely). The channel
// is closed when Stop is called. ADR-0023 D6B.
func (w *InterviewWorker) ReadyChan() <-chan string { return w.readyCh }

// WaitForAgentReadiness blocks until all named agent IDs appear on ReadyChan
// or timeout fires. Returns nil when all agents are ready; an error describing
// the missing agents on timeout. Used by the integration benchmark to replace
// polling ProfileStore at 1-second intervals. ADR-0023 D6B.
func WaitForAgentReadiness(w *InterviewWorker, agentIDs []string, timeout time.Duration) error {
	target := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		target[id] = true
	}
	ready := make(map[string]bool, len(agentIDs))

	deadline := time.After(timeout)
	for len(ready) < len(target) {
		select {
		case id := <-w.ReadyChan():
			if target[id] {
				ready[id] = true
			}
		case <-deadline:
			var missing []string
			for id := range target {
				if !ready[id] {
					missing = append(missing, id)
				}
			}
			return fmt.Errorf("agents not ready after %v: %v", timeout, missing)
		}
	}
	return nil
}

// emitReady publishes AgentReadyEvent on EventBus and notifies ReadyChan.
// Safe to call concurrently. Both sends are non-blocking / nil-safe.
func (w *InterviewWorker) emitReady(agent domain.AgentDefinition, capabilities []string, trustScore float64, interviewMs int64) {
	if w.EventBus != nil {
		_ = w.EventBus.Publish(domain.AgentReadyEvent{
			AgentID:      agent.ID,
			SourceHash:   agent.SourceHash,
			TrustScore:   trustScore,
			Capabilities: capabilities,
			InterviewMs:  interviewMs,
		})
	}
	if w.readyCh != nil {
		select {
		case w.readyCh <- agent.ID:
		default:
		}
	}
}

// NewInterviewWorkerWithRequester creates an InterviewWorker that also calls
// RequestProposalFrom on the agent for each interview scenario.
func NewInterviewWorkerWithRequester(registry ManifestReader, embedder domain.Embedder, store ProfileStore, updater AgentUpdater, requester domain.ProposalRequester) *InterviewWorker {
	w := NewInterviewWorker(registry, embedder, store, updater)
	w.Requester = requester
	return w
}

// Start launches w.PoolSize goroutines draining w.queue. Blocks until ctx is cancelled.
func (w *InterviewWorker) Start(ctx context.Context) error {
	slog.Info("InterviewWorker: starting pool", "size", w.PoolSize)

	g, gCtx := errgroup.WithContext(ctx)
	for i := 0; i < w.PoolSize; i++ {
		g.Go(func() error {
			for {
				select {
				case <-gCtx.Done():
					return nil
				case agent, ok := <-w.queue:
					if !ok {
						return nil
					}
					if err := w.processAgent(gCtx, agent); err != nil {
						slog.Warn("interview_worker: processAgent failed", "agent_id", agent.ID, "err", err)
					}
				}
			}
		})
	}
	return g.Wait()
}

// Enqueue adds an agent to the processing queue without blocking.
func (w *InterviewWorker) Enqueue(agent domain.AgentDefinition) {
	select {
	case w.queue <- agent:
	default:
	}
}

func (w *InterviewWorker) processAgent(ctx context.Context, agent domain.AgentDefinition) error {
	// Fast-path: TraitTool / TraitDaemon and privileged SYSTEM agents (ADR-0051 Scout)
	// bypass LLM scenario generation. They embed Description directly so they participate
	// in cosine-similarity capability clustering. ADR-0033: TraitDaemon; ADR-0051: a system
	// agent is kernel-invoked directly, so it is VERIFIED BY DEFAULT (no interview).
	if agent.Trait == domain.TraitTool || agent.Trait == domain.TraitDaemon || domain.IsSystemAgent(agent.ID) {
		toolEmbedding, embedErr := w.Embedder.Embed(ctx, agent.Description)
		if embedErr != nil {
			toolEmbedding = []float32{}
		}
		profile := domain.AgentProfile{
			AgentID:                agent.ID,
			SourceHash:             agent.SourceHash,
		Provisional:            false,
		TrustScore:             1.0,
			SuccessRate:            1.0,
			NetworkLatencyMedianMs: 5,
		}
		if err := w.Store.SaveProfile(ctx, agent.ID, agent.SourceHash, toolEmbedding, profile); err != nil {
			return fmt.Errorf("interview_worker: save profile for tool agent %s: %w", agent.ID, err)
		}
		if err := w.Updater.SetProvisional(agent.ID, false); err != nil {
			return fmt.Errorf("interview_worker: set provisional=false for tool agent %s: %w", agent.ID, err)
		}
		if w.SweepTrigger != nil {
			w.SweepTrigger.TriggerSweep()
		}
		caps := []string{}
		if m, mErr := w.Registry.GetManifest(ctx, agent.ID); mErr == nil && m != nil {
			caps = m.Tools
		}
		w.emitReady(agent, caps, 1.0, 0)
		return nil
	}

	if agent.Runtime == domain.RuntimeA2A {
		return w.processA2AAgent(ctx, agent)
	}

	// Graded interview (ADR-0037): an LLM asks questions, the agent answers them
	// for real, and the answers are graded into a cold-start routing prior. nil
	// Examiner ⇒ the legacy bid-only path below (backward compatible).
	if w.Examiner != nil {
		return w.processGradedInterview(ctx, agent)
	}

	manifest, err := w.Registry.GetManifest(ctx, agent.ID)
	if err != nil || manifest == nil {
		manifest = &domain.AgentManifest{}
	}

	scenarios := buildScenarios(manifest)

	records, _ := w.Store.GetJudicialRecords(ctx, agent.ID, agent.SourceHash, 3)
	if len(records) > 0 {
		scenarios = append(scenarios, fmt.Sprintf("Address the following critique from a previous version: %s", records[0]))
	}

	combined := strings.Join(scenarios, "\n")
	embedding, err := w.Embedder.Embed(ctx, combined)
	if err != nil {
		return fmt.Errorf("interview_worker: embed scenarios for agent %s: %w", agent.ID, err)
	}

	var seededSuccessRate, seededTrustScore float64
	priorProfile, _ := w.Store.GetProfile(ctx, agent.ID, agent.SourceHash)
	if priorProfile != nil {
		decay, decayErr := w.computeDecay(ctx, manifest, priorProfile)
		if decayErr == nil {
			seededSuccessRate = priorProfile.SuccessRate * decay
			seededTrustScore = priorProfile.TrustScore * decay
		}
	}

	if w.Requester != nil {
		var hint float32
		if priorProfile != nil {
			hint = float32(priorProfile.TrustScore)
			if hint < 0.0 {
				hint = 0.0
			}
			if hint > 1.0 {
				hint = 1.0
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for i, scenario := range scenarios {
			req := domain.ProposalRequest{
				TaskID:         fmt.Sprintf("%s-interview-%d", agent.ID, i),
				Description:    scenario,
				Context:        "interview",
				Deadline:       deadline,
				ConfidenceHint: hint,
			}
			start := time.Now()
			resp, respErr := w.Requester.RequestProposalFrom(ctx, agent, req)
			latencyMs := int(time.Since(start).Milliseconds())

			if respErr == nil && w.EventWriter != nil {
				_ = w.EventWriter.WriteTaskEvent(domain.TaskEvent{
					TaskID:           fmt.Sprintf("%s-interview-%d", agent.ID, i),
					AgentID:          agent.ID,
					SourceHash:       agent.SourceHash,
					BidConfidence:    float64(resp.Confidence),
					NetworkLatencyMs: latencyMs,
					Timestamp:        time.Now(),
					Verified:         false,
				})
			}
		}
	}

	profile := domain.AgentProfile{
		AgentID:     agent.ID,
		SourceHash:  agent.SourceHash,
		Provisional: false,
		SuccessRate: seededSuccessRate,
		TrustScore:  seededTrustScore,
	}

	if err := w.Store.SaveProfile(ctx, agent.ID, agent.SourceHash, embedding, profile); err != nil {
		return fmt.Errorf("interview_worker: save profile for agent %s: %w", agent.ID, err)
	}

	if err := w.Updater.SetProvisional(agent.ID, false); err != nil {
		return fmt.Errorf("interview_worker: set provisional=false for agent %s: %w", agent.ID, err)
	}
	if w.SweepTrigger != nil {
		w.SweepTrigger.TriggerSweep()
	}
	w.emitReady(agent, manifest.Tools, seededTrustScore, 0)

	return nil
}

// processGradedInterview runs the LLM-driven, answer-graded interview and seeds
// the agent's cold-start routing prior from the mean grade (ADR-0037). It is the
// upgraded cognitive path used when an Examiner is wired.
func (w *InterviewWorker) processGradedInterview(ctx context.Context, agent domain.AgentDefinition) error {
	manifest, mErr := w.Registry.GetManifest(ctx, agent.ID)
	if mErr != nil || manifest == nil {
		manifest = &domain.AgentManifest{}
	}

	result := w.Examiner.Run(ctx, agent, manifest)

	// The profile embedding is the real Q/A transcript — a richer capability vector
	// than the templated-scenario text, so the Layer-2 semantic gate matches tasks
	// against what the agent actually did, not boilerplate.
	embedSource := result.Transcript()
	if strings.TrimSpace(embedSource) == "" {
		embedSource = agent.Description
	}
	embedding, embErr := w.Embedder.Embed(ctx, embedSource)
	if embErr != nil {
		return fmt.Errorf("interview_worker: embed transcript for agent %s: %w", agent.ID, embErr)
	}

	// Cold-start prior = graded performance, blended with prior-version decay when
	// this is a re-interview (so proven trust is not discarded on a minor update).
	score := clamp(result.MeanScore, 0, 1)
	seededSuccessRate, seededTrustScore := score, score
	if prior, _ := w.Store.GetProfile(ctx, agent.ID, agent.SourceHash); prior != nil {
		if decay, dErr := w.computeDecay(ctx, manifest, prior); dErr == nil {
			seededSuccessRate = 0.5*score + 0.5*prior.SuccessRate*decay
			seededTrustScore = 0.5*score + 0.5*prior.TrustScore*decay
		}
	}

	// Record a VERIFIED interview TaskEvent so the grade flows through the normal
	// ProfileAggregator → TrustScore path and is auditable.
	if w.EventWriter != nil {
		_ = w.EventWriter.WriteTaskEvent(domain.TaskEvent{
			TaskID:        fmt.Sprintf("%s-interview", agent.ID),
			AgentID:       agent.ID,
			SourceHash:    agent.SourceHash,
			VerifierScore: score,
			Verified:      true,
			Timestamp:     time.Now(),
		})
	}

	// Seed the EFE belief store too, when wired (nil today — ADR-0037 deferred).
	if w.BeliefSeeder != nil {
		if err := w.BeliefSeeder.SeedFromInterview(ctx, agent.ID, embedding, score); err != nil {
			slog.Warn("interview_worker: belief seed failed", "agent_id", agent.ID, "err", err)
		}
	}

	profile := domain.AgentProfile{
		AgentID:     agent.ID,
		SourceHash:  agent.SourceHash,
		Provisional: false,
		SuccessRate: seededSuccessRate,
		TrustScore:  seededTrustScore,
	}
	if err := w.Store.SaveProfile(ctx, agent.ID, agent.SourceHash, embedding, profile); err != nil {
		return fmt.Errorf("interview_worker: save profile for agent %s: %w", agent.ID, err)
	}
	if err := w.Updater.SetProvisional(agent.ID, false); err != nil {
		return fmt.Errorf("interview_worker: set provisional=false for agent %s: %w", agent.ID, err)
	}
	if w.SweepTrigger != nil {
		w.SweepTrigger.TriggerSweep()
	}
	slog.Info("interview_worker: graded interview complete",
		"agent_id", agent.ID, "mean_score", score, "questions", len(result.Questions))
	w.emitReady(agent, manifest.Tools, seededTrustScore, 0)
	return nil
}

func buildScenarios(manifest *domain.AgentManifest) []string {
	if len(manifest.Tools) == 0 {
		return []string{"Describe how you would handle a general-purpose task with no specific tooling."}
	}
	tools := manifest.Tools
	if len(tools) > 3 {
		tools = tools[:3]
	}
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i] = fmt.Sprintf("How would you handle a task requiring tool: %s?", tool)
	}
	return out
}

func (w *InterviewWorker) computeDecay(ctx context.Context, newManifest *domain.AgentManifest, priorProfile *domain.AgentProfile) (float64, error) {
	if newManifest.ReleaseNotes == "" {
		return DefaultDecayClampMin, nil
	}
	newVec, err := w.Embedder.Embed(ctx, newManifest.ReleaseNotes)
	if err != nil {
		return DefaultDecayClampMin, fmt.Errorf("computeDecay: embed new release notes: %w", err)
	}
	oldVec, err := w.Embedder.Embed(ctx, priorProfile.SourceHash)
	if err != nil {
		return DefaultDecayClampMin, fmt.Errorf("computeDecay: embed prior source hash: %w", err)
	}
	distance := w.Store.EmbeddingDistance(oldVec, newVec)
	return clamp(1.0-distance, DefaultDecayClampMin, DefaultDecayClampMax), nil
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func (w *InterviewWorker) processA2AAgent(ctx context.Context, agent domain.AgentDefinition) error {
	card, err := w.CardFetcher.FetchCard(ctx, agent.A2AEndpoint)
	if err != nil {
		return fmt.Errorf("interview_worker: FetchCard for A2A agent %s: %w", agent.ID, err)
	}

	parts := make([]string, 0, 1+len(card.Skills))
	parts = append(parts, card.Description)
	for _, skill := range card.Skills {
		parts = append(parts, skill.Description)
	}
	cardText := strings.Join(parts, "\n")

	embedding, err := w.Embedder.Embed(ctx, cardText)
	if err != nil {
		return fmt.Errorf("interview_worker: embed card for A2A agent %s: %w", agent.ID, err)
	}

	var seededSuccessRate, seededTrustScore float64
	priorProfile, _ := w.Store.GetProfile(ctx, agent.ID, agent.SourceHash)
	if priorProfile != nil {
		syntheticManifest := &domain.AgentManifest{ReleaseNotes: cardText}
		decay, decayErr := w.computeDecay(ctx, syntheticManifest, priorProfile)
		if decayErr == nil {
			seededSuccessRate = priorProfile.SuccessRate * decay
			seededTrustScore = priorProfile.TrustScore * decay
		}
	}

	profile := domain.AgentProfile{
		AgentID:     agent.ID,
		SourceHash:  agent.SourceHash,
		Provisional: true,
		SuccessRate: seededSuccessRate,
		TrustScore:  seededTrustScore,
	}

	if err := w.Store.SaveProfile(ctx, agent.ID, agent.SourceHash, embedding, profile); err != nil {
		return fmt.Errorf("interview_worker: save profile for A2A agent %s: %w", agent.ID, err)
	}

	if err := w.Updater.SetProvisional(agent.ID, false); err != nil {
		return fmt.Errorf("interview_worker: set provisional=false for A2A agent %s: %w", agent.ID, err)
	}
	if w.SweepTrigger != nil {
		w.SweepTrigger.TriggerSweep()
	}
	w.emitReady(agent, []string{}, seededTrustScore, 0)

	return nil
}
