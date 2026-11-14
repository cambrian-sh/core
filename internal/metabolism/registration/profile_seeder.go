// Package registration turns a newly registered (or changed) agent into a routable
// one: it builds the agent's profile vector DETERMINISTICALLY from what the agent
// declares, saves a profile on the neutral prior, clears Provisional and announces
// the agent as ready.
//
// It replaces the ADR-0037 graded LLM interview, deleted 2026-11-05 by owner
// decision (ADR-0139 Q3; T-20261105-delete-the-llm-agent-interview-now-owner). No
// LLM is called, no question is asked and no agent process is booted here. An
// agent's merit comes from VERIFIED step outcomes, aggregated into the profile under
// its real source hash (ADR-0139 D6; unverified outcomes are neutral 0.5). ADR-0139
// D5 qualification (versioned probe suites with deterministic checkers) is the
// future replacement for a cold-start signal; it is not built here.
package registration

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"

	"golang.org/x/sync/errgroup"
)

const (
	// DefaultPoolSize is the number of concurrent seeding goroutines. Seeding is
	// one embedding call per agent, so the pool only bounds embedder concurrency.
	DefaultPoolSize = 5

	// NeutralPrior is the success/trust an agent carries when it has no evidence:
	// the same 0.5 gatekeeper.ScoreMerit assumes for a missing profile (ADR-0139 D6).
	NeutralPrior = 0.5

	// queueCapacity bounds pending registrations. Enqueue never blocks: an agent
	// dropped on a full queue stays Provisional — still routable, see the
	// Gatekeeper — and the next boot's profile backfill re-enqueues it.
	queueCapacity = 100
)

// ManifestReader fetches an agent's manifest by ID.
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// ProfileStore is what the seeder needs from the profile store: read the existing
// profile of this exact version, and write the seeded one — exactly
// domain.VerifierProfileStore, so an alias of the port rather than a restated
// copy of its method set (AGENTS.md §3.1).
type ProfileStore = domain.VerifierProfileStore

// AgentUpdater marks an agent as no longer Provisional in the registry.
type AgentUpdater interface {
	SetProvisional(agentID string, provisional bool) error
}

// ProfileSeeder is the background pool that drives the Provisional → ready
// transition for newly registered or updated agents.
type ProfileSeeder struct {
	Registry ManifestReader
	Embedder domain.Embedder
	Store    ProfileStore
	Updater  AgentUpdater
	// CardFetcher, when set, lets an A2A agent's vector be built from its Agent Card
	// (description plus every skill description), which is richer than the
	// one-line description stored at registration. nil ⇒ the stored description.
	// This is card INGESTION — no LLM, no exam (ADR-0139 D4).
	CardFetcher domain.CardFetcher
	// EventBus receives AgentReadyEvent after every Provisional→ready transition
	// (ADR-0023 D6A / ADR-0030). nil-safe.
	EventBus domain.EventBus
	PoolSize int

	queue   chan domain.AgentDefinition
	readyCh chan string // ADR-0023 D6B: non-blocking; capacity 32
}

// NewProfileSeeder creates a ProfileSeeder with the default pool size.
func NewProfileSeeder(registry ManifestReader, embedder domain.Embedder, store ProfileStore, updater AgentUpdater) *ProfileSeeder {
	return &ProfileSeeder{
		Registry: registry,
		Embedder: embedder,
		Store:    store,
		Updater:  updater,
		PoolSize: DefaultPoolSize,
		queue:    make(chan domain.AgentDefinition, queueCapacity),
		readyCh:  make(chan string, 32),
	}
}

// ReadyChan returns a channel that receives agent IDs as they become ready. The
// send is non-blocking: if nobody is reading, events are dropped. ADR-0023 D6B.
func (s *ProfileSeeder) ReadyChan() <-chan string { return s.readyCh }

// WaitForAgentReadiness blocks until all named agent IDs appear on ReadyChan or
// timeout fires. Used by the integration benchmark instead of polling the store.
func WaitForAgentReadiness(s *ProfileSeeder, agentIDs []string, timeout time.Duration) error {
	target := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		target[id] = true
	}
	ready := make(map[string]bool, len(agentIDs))
	deadline := time.After(timeout)
	for len(ready) < len(target) {
		select {
		case id := <-s.ReadyChan():
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

// Start launches PoolSize goroutines draining the queue. Blocks until ctx is
// cancelled; every goroutine it starts has returned when it does.
func (s *ProfileSeeder) Start(ctx context.Context) error {
	size := s.PoolSize
	if size <= 0 {
		size = DefaultPoolSize
	}
	slog.Info("ProfileSeeder: starting pool", "size", size)
	g, gCtx := errgroup.WithContext(ctx)
	for range size {
		g.Go(func() error {
			for {
				select {
				case <-gCtx.Done():
					return nil
				case agent, ok := <-s.queue:
					if !ok {
						return nil
					}
					if err := s.Seed(gCtx, agent); err != nil {
						slog.Warn("profile_seeder: seeding failed; agent stays provisional until the next backfill",
							"agent_id", agent.ID, "err", err)
					}
				}
			}
		})
	}
	return g.Wait()
}

// Enqueue adds an agent to the seeding queue without blocking.
func (s *ProfileSeeder) Enqueue(agent domain.AgentDefinition) {
	select {
	case s.queue <- agent:
	default:
		slog.Warn("profile_seeder: queue full; agent stays provisional until the next backfill",
			"agent_id", agent.ID)
	}
}

// Seed makes one agent ready. Exported so the composition root and tests can seed
// synchronously; the pool calls it for every queued agent.
func (s *ProfileSeeder) Seed(ctx context.Context, agent domain.AgentDefinition) error {
	// TraitModel agents (`llm:<id>`) are inference providers reached through the
	// model gateway, not processes. Deliberately NO profile and NO vector: a missing
	// profile is the Gatekeeper's neutral prior for model sub-selection
	// (FindModelCandidates), and a description vector would let a model agent pass
	// Layer 2 into a user-task slate it has never been part of.
	if agent.Trait == domain.TraitModel {
		if agent.Provisional {
			if err := s.Updater.SetProvisional(agent.ID, false); err != nil {
				return fmt.Errorf("profile_seeder: set provisional=false for model agent %s: %w", agent.ID, err)
			}
		}
		s.emitReady(agent, []string{}, NeutralPrior)
		return nil
	}

	manifest, mErr := s.Registry.GetManifest(ctx, agent.ID)
	if mErr != nil || manifest == nil {
		manifest = &domain.AgentManifest{}
	}

	text := s.vectorText(ctx, agent, manifest)
	embedding, err := s.Embedder.Embed(ctx, text)
	if err != nil || len(embedding) == 0 {
		// A profile with no vector is invisible to the Gatekeeper's Layer-2 semantic
		// gate. Do NOT mark the agent ready: leave it Provisional (which the Gatekeeper
		// routes on the neutral prior, bypassing L2) and let the boot backfill re-seed
		// it once the embedder is healthy.
		return fmt.Errorf("profile_seeder: empty embedding for agent %s (embedder down?): %w", agent.ID, err)
	}

	profile := s.seededProfile(ctx, agent)
	if err := s.Store.SaveProfile(ctx, agent.ID, agent.SourceHash, embedding, profile); err != nil {
		return fmt.Errorf("profile_seeder: save profile for agent %s: %w", agent.ID, err)
	}
	if err := s.Updater.SetProvisional(agent.ID, false); err != nil {
		return fmt.Errorf("profile_seeder: set provisional=false for agent %s: %w", agent.ID, err)
	}
	caps := preferCaps(manifest.Capabilities, manifest.Tools)
	if agent.Runtime == domain.RuntimeA2A {
		caps = []string{}
	}
	slog.Info("profile_seeder: agent routable",
		"agent_id", agent.ID, "source_hash", agent.SourceHash,
		"success_rate", profile.SuccessRate, "trust_score", profile.TrustScore)
	s.emitReady(agent, caps, profile.TrustScore)
	return nil
}

// seededProfile is the profile written for this version.
//
//   - Tool, daemon and privileged system agents are verified by default (ADR-0033,
//     ADR-0051): they are kernel-invoked or deterministic, so they carry full trust.
//   - Every other agent starts on the NEUTRAL prior (SR = Trust = 0.5) — new agents
//     and changed agents (a new source hash) alike; nothing is inherited from a
//     previous version (owner decision 2026-11-05).
//   - A re-seed of the SAME version (boot backfill of a still-provisional agent, or
//     self-heal of a missing vector) keeps the outcomes this version has already
//     earned: only the vector is refreshed.
func (s *ProfileSeeder) seededProfile(ctx context.Context, agent domain.AgentDefinition) domain.AgentProfile {
	if agent.Trait == domain.TraitDaemon || domain.IsSystemDefinition(agent) {
		return domain.AgentProfile{
			AgentID:                agent.ID,
			SourceHash:             agent.SourceHash,
			TrustScore:             1.0,
			SuccessRate:            1.0,
			NetworkLatencyMedianMs: 5,
		}
	}
	if existing, err := s.Store.GetProfile(ctx, agent.ID, agent.SourceHash); err == nil && existing != nil {
		kept := *existing
		kept.AgentID, kept.SourceHash, kept.Provisional = agent.ID, agent.SourceHash, false
		return kept
	}
	return domain.AgentProfile{
		AgentID:     agent.ID,
		SourceHash:  agent.SourceHash,
		SuccessRate: NeutralPrior,
		TrustScore:  NeutralPrior,
	}
}

// vectorText is the deterministic text the profile vector is embedded from — the
// Layer-2 semantic gate matches task descriptions against it.
//
// It is the agent's own description (what the disable_interviews path and the tool
// fast path always used). An A2A agent's Agent Card adds its skill descriptions
// when a CardFetcher is wired. An agent with no description falls back to its
// declared capabilities, then its name, so the vector is never empty text.
// ADR-0139 D4 (per-capability vectors from description plus example tasks) is
// Phase 3; L2 calibration is a Phase 3 item too.
func (s *ProfileSeeder) vectorText(ctx context.Context, agent domain.AgentDefinition, manifest *domain.AgentManifest) string {
	if agent.Runtime == domain.RuntimeA2A && s.CardFetcher != nil && agent.A2AEndpoint != "" {
		card, err := s.CardFetcher.FetchCard(ctx, agent.A2AEndpoint)
		if err == nil && card != nil {
			parts := make([]string, 0, 1+len(card.Skills))
			if d := strings.TrimSpace(card.Description); d != "" {
				parts = append(parts, d)
			}
			for _, skill := range card.Skills {
				if d := strings.TrimSpace(skill.Description); d != "" {
					parts = append(parts, d)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		} else if err != nil {
			slog.Warn("profile_seeder: agent card fetch failed; embedding the stored description",
				"agent_id", agent.ID, "err", err)
		}
	}
	if d := strings.TrimSpace(agent.Description); d != "" {
		return d
	}
	if caps := preferCaps(manifest.Capabilities, manifest.Tools); len(caps) > 0 {
		return strings.Join(caps, ", ")
	}
	if agent.Name != "" {
		return agent.Name
	}
	return agent.ID
}

// preferCaps returns manifest-declared capabilities as the single source of an
// agent's advertised capabilities (ROUTE-03), falling back to the tool list only
// for legacy agents that predate the capabilities field.
func preferCaps(capabilities, tools []string) []string {
	if len(capabilities) > 0 {
		return capabilities
	}
	if tools == nil {
		return []string{}
	}
	return tools
}

// emitReady publishes AgentReadyEvent on EventBus and notifies ReadyChan. Both
// sends are non-blocking / nil-safe.
func (s *ProfileSeeder) emitReady(agent domain.AgentDefinition, capabilities []string, trustScore float64) {
	if s.EventBus != nil {
		// Best-effort: the in-memory bus only fails on a nil event, and a lost ready
		// event costs only the operator feed's freshness (the roster latch seeds from
		// the manifest anyway).
		_ = s.EventBus.Publish(domain.AgentReadyEvent{
			AgentID:      agent.ID,
			SourceHash:   agent.SourceHash,
			TrustScore:   trustScore,
			Capabilities: capabilities,
		})
	}
	if s.readyCh != nil {
		select {
		case s.readyCh <- agent.ID:
		default:
		}
	}
}
