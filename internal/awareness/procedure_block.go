package awareness

import (
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// buildProcedureBlock renders the <ProcedureLTM> block (ADR-0094 D5).
//
// The framing is deliberately ADVISORY. A procedure is DESCRIPTIVE — it says how this
// kind of work has gone, not how it must go — and D6 makes it planner input rather than
// authority. The prompt says so explicitly, because a numbered list of steps reads as an
// instruction unless told otherwise, and a planner that treats a routine as a directive
// has turned induced memory into a control channel.
//
// Steps name CAPABILITIES, never agents (D2). That is what keeps a retrieved routine a
// suggested plan SHAPE rather than a pre-decided assignment: the Gatekeeper still filters
// and the Dispatcher still selects. A routine naming agents would be the Zero-Hardcode
// Rule defeated by a database row rather than by a Go switch.
//
// Confidence and observation count are surfaced so the planner can weigh a
// well-corroborated routine differently from a barely-seen one — the same reason a
// precedent carries its similarity.
func buildProcedureBlock(procedures []domain.Procedure) string {
	if len(procedures) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<ProcedureLTM>\n")
	sb.WriteString("  <!-- How this kind of work has gone here before. ADVISORY: adapt or\n")
	sb.WriteString("       ignore this if the situation differs. Steps name required\n")
	sb.WriteString("       CAPABILITIES, not agents; selection remains the auction's. -->\n")
	for _, p := range procedures {
		fmt.Fprintf(&sb, "  <routine confidence=%q observed=\"%d\">\n",
			fmt.Sprintf("%.2f", p.Confidence), p.SampleCount)
		fmt.Fprintf(&sb, "    <situation>%s</situation>\n", p.Trigger)
		sb.WriteString("    <steps>\n")
		for _, st := range p.Steps {
			fmt.Fprintf(&sb, "      <step capabilities=%q>%s</step>\n",
				strings.Join(st.RequiredCapabilities, ","), st.Intent)
		}
		sb.WriteString("    </steps>\n")
		sb.WriteString("  </routine>\n")
	}
	sb.WriteString("</ProcedureLTM>\n")
	return sb.String()
}
