package clusterer

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// AgentEmbedding carries the data needed by the clusterer for one agent.
type AgentEmbedding struct {
	AgentID     string
	SourceHash  string
	Embedding   []float32
	Description string
	Trait       string // "tool" | "cognitive" | "model" — model agents are excluded from clustering
}

// AgentSource fetches the full set of agent embeddings from the profile store.
type AgentSource interface {
	GetAllAgentEmbeddings(ctx context.Context) ([]AgentEmbedding, error)
}

// ClusterStore persists capability labels and cluster names.
type ClusterStore interface {
	SetCapabilities(agentID string, caps []string) error
	SetClusterName(repID string, name string) error
	GetClusterName(repID string) (string, error)
}

// Generator produces a cluster name from a descriptive prompt.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// CapabilityClusterer groups agents by semantic similarity and assigns
// capability labels via LLM cluster naming.
type CapabilityClusterer struct {
	Source          AgentSource
	Store           ClusterStore
	Generator       Generator
	Threshold       float64
	Epsilon         float64
	MinAgents       int
	IntervalSeconds int

	triggerChan     chan struct{}
	lastFingerprint uint64
	prevReps        map[string]string // clusterKey → previous representativeAgentID
}

// New constructs a CapabilityClusterer with the provided dependencies and config.
func New(source AgentSource, store ClusterStore, gen Generator, threshold, epsilon float64, minAgents int) *CapabilityClusterer {
	return &CapabilityClusterer{
		Source:          source,
		Store:           store,
		Generator:       gen,
		Threshold:       threshold,
		Epsilon:         epsilon,
		MinAgents:       minAgents,
		IntervalSeconds: 3600,
		triggerChan:     make(chan struct{}, 1),
		prevReps:        make(map[string]string),
	}
}

// TriggerSweep satisfies the interview.SweepTrigger interface. The buffered
// channel of size 1 acts as a natural debounce: concurrent calls that arrive
// while a sweep is already queued are silently dropped.
func (c *CapabilityClusterer) TriggerSweep() {
	select {
	case c.triggerChan <- struct{}{}:
	default:
	}
}

// Start blocks, running the two-track execution loop until ctx is cancelled.
// Track 1: event-driven via TriggerSweep (debounced channel).
// Track 2: defensive reconciliation ticker every IntervalSeconds.
func (c *CapabilityClusterer) Start(ctx context.Context) error {
	interval := time.Duration(c.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3600 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	return c.runLoop(ctx, ticker.C)
}

// runLoop is the inner select loop. Accepting tickC separately allows tests to
// inject a synthetic channel instead of a real ticker.
func (c *CapabilityClusterer) runLoop(ctx context.Context, tickC <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.triggerChan:
			if err := c.runSweep(ctx); err != nil {
				slog.Warn("clusterer: runSweep error (trigger)", "err", err)
			}
		case _, ok := <-tickC:
			if !ok {
				return nil
			}
			if err := c.runSweep(ctx); err != nil {
				slog.Warn("clusterer: runSweep error (tick)", "err", err)
			}
		}
	}
}

// runSweep executes one full clustering pass: fetch, cluster, name, write-back.
func (c *CapabilityClusterer) runSweep(ctx context.Context) error {
	agents, err := c.Source.GetAllAgentEmbeddings(ctx)
	if err != nil {
		return fmt.Errorf("clusterer: fetch embeddings: %w", err)
	}

	// Exclude TraitModel agents from clustering.
	eligible := agents[:0]
	for _, a := range agents {
		if a.Trait != "model" {
			eligible = append(eligible, a)
		}
	}

	if len(eligible) < c.MinAgents {
		return nil
	}

	fp := computeFingerprint(eligible)
	fingerprintUnchanged := fp == c.lastFingerprint

	clusters := formClusters(eligible, c.Threshold)

	newPrevReps := make(map[string]string, len(clusters))

	for _, members := range clusters {
		clusterKey := clusterSortedKey(members)
		prevRepID := c.prevReps[clusterKey]
		repID := determineRepresentative(members, prevRepID, c.Epsilon)
		newPrevReps[clusterKey] = repID

		if len(members) == 1 {
			if err := c.Store.SetCapabilities(members[0].AgentID, nil); err != nil {
				return fmt.Errorf("clusterer: SetCapabilities singleton %s: %w", members[0].AgentID, err)
			}
			continue
		}

		// Resolve cluster name: cached if fingerprint unchanged, otherwise LLM.
		var clusterName string
		if fingerprintUnchanged {
			cached, _ := c.Store.GetClusterName(repID)
			if cached != "" {
				clusterName = cached
			}
		}
		if clusterName == "" {
			prompt := buildNamingPrompt(members)
			generated, genErr := c.Generator.Generate(ctx, prompt)
			if genErr != nil {
				return fmt.Errorf("clusterer: name cluster: %w", genErr)
			}
			// Guard against malformed naming responses (error blobs, JSON, junk)
			// leaking into the Planner prompt as a cluster label.
			clusterName = sanitizeClusterName(generated)
			if clusterName == "" {
				clusterName = fallbackClusterName(repID)
				slog.WarnContext(ctx, "clusterer: unusable cluster name, using fallback",
					slog.String("event", "cluster_name_fallback"),
					slog.String("rep_id", repID),
					slog.String("raw_preview", preview(generated, 80)))
			}
		}

		caps := []string{clusterName}
		for _, m := range members {
			if err := c.Store.SetCapabilities(m.AgentID, caps); err != nil {
				return fmt.Errorf("clusterer: SetCapabilities %s: %w", m.AgentID, err)
			}
		}
		if err := c.Store.SetClusterName(repID, clusterName); err != nil {
			return fmt.Errorf("clusterer: SetClusterName %s: %w", repID, err)
		}
	}

	c.lastFingerprint = fp
	c.prevReps = newPrevReps
	return nil
}

// ── Pure helpers ─────────────────────────────────────────────────────────────

func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// formClusters implements single-linkage transitive closure using union-find.
func formClusters(agents []AgentEmbedding, threshold float64) [][]AgentEmbedding {
	n := len(agents)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}

	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(x, y int) {
		px, py := find(x), find(y)
		if px != py {
			parent[px] = py
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cosineSimilarity(agents[i].Embedding, agents[j].Embedding) >= threshold {
				union(i, j)
			}
		}
	}

	groups := make(map[int][]AgentEmbedding)
	for i, a := range agents {
		root := find(i)
		groups[root] = append(groups[root], a)
	}

	result := make([][]AgentEmbedding, 0, len(groups))
	for _, g := range groups {
		result = append(result, g)
	}
	return result
}

// determineRepresentative selects the agent closest to the cluster centroid,
// retaining the previous representative unless the challenger exceeds it by epsilon.
func determineRepresentative(members []AgentEmbedding, previousRepID string, epsilon float64) string {
	var bestID string
	var bestScore, prevRepScore float64 = -1, -1

	for _, m := range members {
		score := avgSimilarity(m, members)
		if m.AgentID == previousRepID {
			prevRepScore = score
		}
		if score > bestScore {
			bestScore = score
			bestID = m.AgentID
		}
	}

	if prevRepScore >= 0 && (bestScore-prevRepScore) < epsilon {
		return previousRepID
	}
	return bestID
}

func avgSimilarity(target AgentEmbedding, members []AgentEmbedding) float64 {
	if len(members) <= 1 {
		return 1.0
	}
	var sum float64
	var count int
	for _, m := range members {
		if m.AgentID == target.AgentID {
			continue
		}
		sum += cosineSimilarity(target.Embedding, m.Embedding)
		count++
	}
	if count == 0 {
		return 1.0
	}
	return sum / float64(count)
}

// computeFingerprint produces a FNV-1a hash over sorted agentID+sourceHash pairs.
func computeFingerprint(agents []AgentEmbedding) uint64 {
	sorted := make([]AgentEmbedding, len(agents))
	copy(sorted, agents)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].AgentID < sorted[j].AgentID
	})

	h := fnv.New64a()
	for _, a := range sorted {
		_, _ = h.Write([]byte(a.AgentID))
		_, _ = h.Write([]byte(a.SourceHash))
	}
	return h.Sum64()
}

// clusterSortedKey produces a stable string key for a cluster (sorted agent IDs).
func clusterSortedKey(members []AgentEmbedding) string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.AgentID
	}
	sort.Strings(ids)
	key := ""
	for _, id := range ids {
		key += id + "|"
	}
	return key
}

func buildNamingPrompt(members []AgentEmbedding) string {
	var agentsB strings.Builder
	for _, m := range members {
		if m.Description != "" {
			agentsB.WriteString("- ")
			agentsB.WriteString(m.Description)
			agentsB.WriteByte('\n')
		}
	}
	return domain.PromptBuild(
		domain.PromptSystem("You are a capability taxonomy expert."),
		domain.PromptContext("Agents in this cluster:\n"+agentsB.String()),
		domain.PromptTask("Name this cluster of agents with a short capability label."),
		domain.PromptOutputSchemaString(40, "A short, 1-3 word capability label in lowercase_with_underscores."),
	)
}
