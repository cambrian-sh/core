package memory

import (
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// BuildLTMContext constructs the typed XML-tag LTM prompt section from a prior plan
// and the enrichment retrieved by WorkspaceStage. Returns empty string when all inputs
// are empty — callers skip the injection entirely on empty return. ADR-0025.
func BuildLTMContext(plan *domain.PlanLTMEntry, enrichment domain.LTMEnrichment) string {
	var sb strings.Builder

	if plan != nil && plan.PlanJSON != "" {
		fmt.Fprintf(&sb, "<PlanLTM similarity=\"%.2f\" confidence=\"%.2f\" outcome=\"%s\" replan_count=\"%d\">\n",
			plan.Similarity, plan.Confidence, plan.Outcome, plan.ReplanCount)
		sb.WriteString("  ")
		sb.WriteString(plan.PlanJSON)
		sb.WriteString("\n</PlanLTM>\n")
	}

	if len(enrichment.Facts) > 0 {
		sb.WriteString("<FactLTM>\n")
		for i, r := range enrichment.Facts {
			fmt.Fprintf(&sb, "  <fact id=\"%d\" activation=\"%.2f\" relevance=\"%.2f\">%s</fact>\n",
				i, r.Document.ActivationStrength, r.RawScore, r.Document.Text)
		}
		sb.WriteString("</FactLTM>\n")
	}

	if len(enrichment.Negatives) > 0 {
		sb.WriteString("<NegativeLTM>\n")
		for _, r := range enrichment.Negatives {
			agentID, _ := r.Document.Metadata["agent_id"].(string)
			fmt.Fprintf(&sb, "  <failure agent=\"%s\">%s</failure>\n", agentID, r.Document.Text)
		}
		sb.WriteString("</NegativeLTM>\n")
	}

	return sb.String()
}
