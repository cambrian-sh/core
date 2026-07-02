package awareness

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ConsolidatorConfig holds the parameters for session consolidation.
type ConsolidatorConfig struct {
	MaxInputChars int
	TTLDays       int
}

const consolidationRole = "You are the Cambrian System Consolidator — the system's sleep spindle. Your task is to transform raw Synaptic Traces into permanent Knowledge."

const consolidationRules = `KNOWLEDGE PRESERVATION PROTOCOL:
- NEVER generalize, paraphrase, or delete technical data listed in <Context> under "Critical Data".
- Mark any user instruction containing "always" or "never" as High-Priority Semantic Memory (type: preference).
- If the user explicitly corrected a behavior, record it as type "preference" with confidence_boost >= 0.3.

ABSTRACTION TASK:
- Convert concrete steps into goal-oriented skeletons.
- Replace ALL variables (file paths, IPs, names, ports) with {{variable_name}} placeholders.
- Each template needs an intent_key — the short trigger phrase for recall.

ROOT CAUSE ANALYSIS:
- For every negative memory (type: negative), classify WHY the failure occurred:
  connection_error | logic_error | permission_denied | timeout | resource_exhaustion | user_intervention | unknown`

const consolidationSchema = `{
  "type": "object",
  "properties": {
    "semantic_memories":    { "type": "array" },
    "procedural_templates": { "type": "array" },
    "cleanup_actions":      { "type": "array" },
    "session_summary":      { "type": "string" }
  },
  "required": ["semantic_memories", "procedural_templates", "cleanup_actions", "session_summary"]
}`

var consolidationPromptHash = domain.PromptHashOf(consolidationRole + consolidationRules + consolidationSchema)

func init() {
	domain.PromptRegistry[consolidationPromptHash] = domain.PromptEntry{
		ID:      "memory.consolidation",
		Version: "1.0.0",
		Hash:    consolidationPromptHash,
		Schema:  consolidationSchema,
	}
}

// BuildConsolidationPrompt constructs the LLM prompt for session consolidation.
// It applies significance-based sampling (priority >= 7 events preserved,
// priority 5 capped, priority <= 3 discarded), knowledge preservation
// protocol, and root cause analysis instructions.
func BuildConsolidationPrompt(events []ConsolidationEvent, criticalData []string, profiles string) string {
	var contextParts strings.Builder
	if len(criticalData) > 0 {
		contextParts.WriteString("Critical Data (NEVER delete or paraphrase):\n")
		for _, d := range criticalData {
			fmt.Fprintf(&contextParts, "  - %s\n", d)
		}
		contextParts.WriteString("\n")
	}
	contextParts.WriteString("Session Event Log (significance-sampled):\n")
	for _, ev := range events {
		fmt.Fprintf(&contextParts, "[%s] %s: %s\n", ev.Type, ev.AgentID, truncate(ev.Payload, 200))
	}

	return domain.PromptBuild(
		domain.PromptSystem(consolidationRole, consolidationRules),
		domain.PromptContext(contextParts.String()),
		domain.PromptTask("Compress the session events into structured long-term knowledge."),
		domain.PromptOutputSchemaJSON(consolidationSchema),
	)
}

// ConsolidationEvent is a simplified event for the consolidation prompt.
type ConsolidationEvent struct {
	Type      string
	Payload   string
	AgentID   string
	Timestamp string
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

const reconsolidationRole = "You are Cambrian's memory reconsolidation judge."
const reconsolidationRules = "Two LTM documents contradict each other. Identify which is more credible based on activation_strength and access_count."
const reconsolidationSchema = `{"type":"object","properties":{"winner_id":{"type":"string"},"explanation":{"type":"string"}},"required":["winner_id","explanation"]}`

var reconsolidationPromptHash = domain.PromptHashOf(reconsolidationRole + reconsolidationRules + reconsolidationSchema)

func init() {
	domain.PromptRegistry[reconsolidationPromptHash] = domain.PromptEntry{
		ID:      "memory.reconsolidation",
		Version: "1.0.0",
		Hash:    reconsolidationPromptHash,
		Schema:  reconsolidationSchema,
	}
}

// BuildReconsolidationPrompt constructs the LLM prompt for contradiction arbitration.
// ADR-0016: called during consolidation to resolve [CONFLICT]-tagged document pairs.
func BuildReconsolidationPrompt(docA, docB domain.Document) string {
	contextContent := fmt.Sprintf(
		"DOCUMENT A:\n  ID: %s\n  Text: %s\n  activation_strength: %.2f\n  access_count: %d\n\nDOCUMENT B:\n  ID: %s\n  Text: %s\n  activation_strength: %.2f\n  access_count: %d",
		docA.ID, docA.Text, docA.ActivationStrength, docA.AccessCount,
		docB.ID, docB.Text, docB.ActivationStrength, docB.AccessCount,
	)
	return domain.PromptBuild(
		domain.PromptSystem(reconsolidationRole, reconsolidationRules),
		domain.PromptContext(contextContent),
		domain.PromptTask("Select the more credible document and provide a brief explanation."),
		domain.PromptOutputSchemaJSON(reconsolidationSchema),
	)
}

// ResolveContradiction calls the Generator with the reconsolidation prompt, parses the winner,
// and downgrades the loser's activation_strength. ADR-0016.
// If a GraphStore is provided, also decays the contradicts edge weight (ADR-0017).
// If logger is non-nil, logs the full feature vector for future model training (ADR-0021).
// Returns the winner ID or empty string on failure.
func ResolveContradiction(ctx context.Context, gen domain.Generator, docA, docB domain.Document, updater func(ctx context.Context, docID string, delta float64) error, gs domain.GraphStore, logger domain.ContradictionLogger) string {
	prompt := BuildReconsolidationPrompt(docA, docB)

	resp, err := gen.Generate(ctx, prompt)
	if err != nil || resp == "" {
		slog.Warn("Consolidator: reconsolidation Generator call failed", "err", err)
		return ""
	}

	winnerID := extractWinnerID(resp)
	if winnerID == "" {
		slog.Warn("Consolidator: reconsolidation parse failure",
			"reconsolidation_parse_failure", true, "raw_response", resp)
		return ""
	}

	// Downgrade the loser.
	var loser domain.Document
	if winnerID == docA.ID {
		loser = docB
	} else if winnerID == docB.ID {
		loser = docA
	} else {
		return ""
	}

	newAS := loser.ActivationStrength - 0.1
	if newAS < 0 {
		newAS = 0
	}
	delta := newAS - loser.ActivationStrength
	if err := updater(ctx, loser.ID, delta); err != nil {
		slog.Warn("Consolidator: failed to update loser activation", "loser_id", loser.ID, "err", err)
		return ""
	}

	slog.Info("Consolidator: contradiction resolved",
		"winner_id", winnerID, "loser_id", loser.ID,
		"loser_old_as", fmt.Sprintf("%.2f", loser.ActivationStrength),
		"loser_new_as", fmt.Sprintf("%.2f", newAS))

	// ADR-0021: Log contradiction resolution feature vector for future model training.
	if logger != nil {
		sim := cosineSimilarity(docA.Embedding.Vector, docB.Embedding.Vector)
		now := time.Now()
		res := domain.ContradictionResolution{
			ResolutionID:       fmt.Sprintf("cr-%s-%s-%d", docA.ID, docB.ID, now.UnixNano()),
			DocAID:             docA.ID,
			DocBID:             docB.ID,
			WinnerID:           winnerID,
			DocAAS:             docA.ActivationStrength,
			DocBAS:             docB.ActivationStrength,
			DocAAccessCount:    docA.AccessCount,
			DocBAccessCount:    docB.AccessCount,
			DocAAgeDays:        int(now.Sub(docA.CreatedAt).Hours() / 24),
			DocBAgeDays:        int(now.Sub(docB.CreatedAt).Hours() / 24),
			SemanticSimilarity: sim,
			Timestamp:          now,
		}
		if err := logger.LogContradiction(res); err != nil {
			slog.Warn("Consolidator: failed to log contradiction resolution", "err", err)
		}
	}

	// ADR-0017: Decay contradict edge weight on resolution.
	if gs != nil {
		currentWeight := float32(0.5)
		edges, err := gs.GetAdjacentEdges(ctx, []string{docA.ID})
		if err == nil {
			for _, e := range edges {
				if e.EdgeType == domain.EdgeContradicts && e.TargetID == docB.ID {
					currentWeight = e.Weight
					break
				}
			}
		}
		if currentWeight > 0 {
			newWeight := currentWeight * 0.5
			if err := gs.UpdateEdgeWeight(ctx, docA.ID, docB.ID, domain.EdgeContradicts, newWeight); err != nil {
				slog.Warn("Consolidator: failed to decay contradicts edge weight", "err", err)
			}
		}
	}

	return winnerID
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func extractWinnerID(resp string) string {
	start := strings.Index(resp, `"winner_id"`)
	if start < 0 {
		return ""
	}
	colon := strings.Index(resp[start:], ":")
	if colon < 0 {
		return ""
	}
	valStart := strings.Index(resp[start+colon:], `"`)
	if valStart < 0 {
		return ""
	}
	valStart += start + colon + 1 // skip the opening quote
	valEnd := strings.Index(resp[valStart+1:], `"`)
	if valEnd < 0 {
		return ""
	}
	return resp[valStart : valStart+valEnd+1]
}
