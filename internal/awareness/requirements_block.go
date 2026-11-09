package awareness

import (
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// RenderRequirementsBlock renders the checklist for planner_input v3's
// <requirements> region (ADR-0145 D5): one line per item with its id, type, text
// and quote (constraints with the effects they forbid), and, on a coverage stage,
// which items are settled and which are still open, with the reason, the missing
// for_each members and any retry instruction. Kernel-written; the quotes are the
// user's words and stay data (the caller neutralises closing tags).
func RenderRequirementsBlock(c *domain.RequirementChecklist, stage *domain.CoverageStage) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, it := range c.Items {
		fmt.Fprintf(&b, "%s [%s] %s (from the request: %q)", it.ID, it.Type, it.Text, it.Quote)
		if len(it.Forbids) > 0 {
			fmt.Fprintf(&b, " forbids: %s", strings.Join(it.Forbids, ", "))
		}
		b.WriteByte('\n')
	}
	if stage == nil {
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "\nStage %d. Earlier stages ran; plan only what the open items below still need.\n", stage.Number)
	if len(stage.Settled) > 0 {
		fmt.Fprintf(&b, "Settled (do not redo): %s\n", strings.Join(stage.Settled, ", "))
	}
	for _, o := range stage.Open {
		fmt.Fprintf(&b, "Open %s", o.ID)
		if o.Reason != "" {
			fmt.Fprintf(&b, ": %s", o.Reason)
		}
		if len(o.Missing) > 0 {
			fmt.Fprintf(&b, ". Members with no result yet, cover exactly these: %s", strings.Join(o.Missing, ", "))
		}
		if o.Retry != "" {
			fmt.Fprintf(&b, ". %s", o.Retry)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
