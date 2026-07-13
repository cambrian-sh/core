package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"

	"github.com/google/uuid"
)

var ErrHippocampusFailure = errors.New("hippocampus: internal infrastructure failure")

// ProceduralTemplateV1 defines the versioned storage format for plans.
type ProceduralTemplateV1 struct {
	Version int                   `json:"version"`
	Plan    *domain.ExecutionPlan `json:"plan"`
}

// punctuation strips everything that is not a letter, digit, space, hyphen, comma, pipe, or technical character.
var punctuation = regexp.MustCompile(`[^\p{L}\d ,|.\-/@]`)

// noopPolicyProvider is used when NewHippocampus receives a nil PolicyProvider.
// It returns the hardcoded defaults that pre-ADR-0027 Hippocampus used, preserving
// existing behaviour without any config dependency.
type noopPolicyProvider struct{}

func (noopPolicyProvider) GetPolicy(_ string) (domain.HippocampusPolicy, bool) {
	return domain.HippocampusPolicy{}, false
}
func (noopPolicyProvider) DefaultPolicy() domain.HippocampusPolicy {
	return domain.HippocampusPolicy{SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 0}
}

// Hippocampus stores and retrieves successful ExecutionPlan patterns (Procedural
// Templates) from pgvector. It is the write and read path for the Awareness layer's
// long-term procedural memory.
type Hippocampus struct {
	store          domain.VectorStore
	embedder       domain.Embedder
	policyProvider domain.PolicyProvider // ADR-0027; never nil after construction
}

// NewHippocampus constructs a Hippocampus. Pass nil for pp to use the pre-ADR-0027
// hardcoded thresholds (0.85 similarity, 0.70 confidence, no age limit) — safe for
// existing callers and tests that have not yet been wired to a real PolicyProvider.
func NewHippocampus(store domain.VectorStore, embedder domain.Embedder, pp domain.PolicyProvider) *Hippocampus {
	if pp == nil {
		pp = noopPolicyProvider{}
	}
	return &Hippocampus{store: store, embedder: embedder, policyProvider: pp}
}

// Store embeds the canonical key for plan and saves it as a DocTypeProceduralTemplate document.
// Failures are logged at WARN and swallowed — a storage failure must never surface as a caller error.
func (h *Hippocampus) Store(ctx context.Context, plan *domain.ExecutionPlan, meanConfidence float64) error {
	if meanConfidence < h.policyProvider.DefaultPolicy().ConfidenceFloor {
		return nil
	}
	key := h.canonicalKey(plan)

	vec, err := h.embedder.Embed(ctx, key)
	if err != nil {
		return fmt.Errorf("%w: failed to embed for storage: %v", ErrHippocampusFailure, err)
	}

	envelope := ProceduralTemplateV1{
		Version: 1,
		Plan:    plan,
	}

	planBytes, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("%w: failed to marshal plan: %v", ErrHippocampusFailure, err)
	}

	doc := &domain.Document{
		ID:           uuid.New().String(),
		DocumentType: domain.DocTypeProceduralTemplate,
		Text:         string(planBytes),
		Embedding:    domain.Embedding{Vector: vec},
		Metadata: map[string]interface{}{
			"mean_auction_confidence": meanConfidence,
			"stored_at":               time.Now().UTC().Format(time.RFC3339),
			"planner_prompt_version":  plan.PlannerPromptVersion,
		},
	}

	if err := h.store.Save(ctx, doc); err != nil {
		slog.Warn("hippocampus: failed to save procedural template", "error", err)
	}
	return nil
}

// Retrieve delegates to RetrieveWithPolicy using the default policy, preserving
// exact pre-ADR-0027 behaviour for all existing callers.
func (h *Hippocampus) Retrieve(ctx context.Context, userInput string) (*domain.ExecutionPlan, float64, float64, error) {
	return h.RetrieveWithPolicy(ctx, userInput, "")
}

// RetrieveWithPolicy searches for the best matching Procedural Template, applying
// the named policy's SimilarityThreshold, ConfidenceFloor, and MaxAgeHours.
// An empty or unknown policyName falls back to the default policy.
func (h *Hippocampus) RetrieveWithPolicy(ctx context.Context, userInput string, policyName string) (*domain.ExecutionPlan, float64, float64, error) {
	policy, ok := h.policyProvider.GetPolicy(policyName)
	if !ok {
		policy = h.policyProvider.DefaultPolicy()
	}

	vec, err := h.embedder.Embed(ctx, normalise(userInput))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: embedding failed: %v", ErrHippocampusFailure, err)
	}

	results, err := h.store.Search(ctx, vec, domain.SearchOptions{
		DocumentType: domain.DocTypeProceduralTemplate,
		TopK:         1,
		Scope:        domain.ScopeSystem, // ADR-0034: procedural-memory retrieval is a kernel read
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: vector search failed: %v", ErrHippocampusFailure, err)
	}

	if len(results) == 0 || results[0].Score < policy.SimilarityThreshold {
		return nil, 0, 0, nil
	}

	conf, _ := results[0].Document.Metadata["mean_auction_confidence"].(float64)
	if conf < policy.ConfidenceFloor {
		return nil, 0, 0, nil
	}

	// ADR-0027: MaxAgeHours enforcement — reject templates that are too old.
	if policy.MaxAgeHours > 0 {
		if storedAtStr, ok := results[0].Document.Metadata["stored_at"].(string); ok {
			if storedAt, err := time.Parse(time.RFC3339, storedAtStr); err == nil {
				if time.Since(storedAt) > time.Duration(policy.MaxAgeHours)*time.Hour {
					return nil, 0, 0, nil
				}
			}
		}
	}

	var envelope ProceduralTemplateV1
	if err := json.Unmarshal([]byte(results[0].Document.Text), &envelope); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: failed to unmarshal plan envelope: %v", ErrHippocampusFailure, err)
	}

	if envelope.Plan != nil {
		if v, ok := results[0].Document.Metadata["planner_prompt_version"].(string); ok {
			envelope.Plan.PlannerPromptVersion = v
		}
	}

	return envelope.Plan, results[0].Score, conf, nil
}

func (h *Hippocampus) canonicalKey(plan *domain.ExecutionPlan) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("intent: %s", plan.Subject))

	for _, step := range plan.Steps {
		sb.WriteString(fmt.Sprintf(" | step: %s", step.Query))
	}

	return normalise(sb.String())
}

func normalise(s string) string {
	lower := strings.ToLower(s)
	clean := punctuation.ReplaceAllString(lower, "")
	return strings.TrimSpace(clean)
}
