package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// The staged planner's data regions (planner_input v4, ADR-0145 D11;
// T-20261108-adr-0145-d11-staged-planner-returns-next-stage). Kernel-written;
// quotes, previews and read results are data (planRequest neutralises closing
// tags of every region).

// RenderStagedRequirements renders <requirements> for a staged call: every item
// with its id, type, text, quote (constraints with what they forbid) and its
// status — open with the kernel's reason, the missing for_each members and any
// retry instruction, or the final status of a settled item. stage nil is the
// first stage: every item but a constraint is open.
func RenderStagedRequirements(c *domain.RequirementChecklist, stage *domain.CoverageStage) string {
	if c == nil {
		return ""
	}
	open := map[string]domain.OpenItem{}
	if stage != nil {
		for _, o := range stage.Open {
			open[o.ID] = o
		}
	}
	var b strings.Builder
	for _, it := range c.Items {
		fmt.Fprintf(&b, "%s [%s] %s (from the request: %q)", it.ID, it.Type, it.Text, it.Quote)
		if len(it.Forbids) > 0 {
			fmt.Fprintf(&b, " forbids: %s", strings.Join(it.Forbids, ", "))
		}
		switch {
		case it.Type == domain.RequirementConstraint:
			b.WriteString(" — status: held while no step makes a forbidden call")
		case stage == nil:
			b.WriteString(" — status: open")
		case stage.SettledAs[it.ID] != "":
			fmt.Fprintf(&b, " — status: %s (settled; do not answer it again)", stage.SettledAs[it.ID])
		default:
			o := open[it.ID]
			b.WriteString(" — status: open")
			if o.Reason != "" {
				fmt.Fprintf(&b, "; %s", o.Reason)
			}
			if len(o.Missing) > 0 {
				fmt.Fprintf(&b, "; members with no result yet, cover exactly these: %s", strings.Join(o.Missing, ", "))
			}
			if o.Retry != "" {
				fmt.Fprintf(&b, "; %s", o.Retry)
			}
		}
		b.WriteByte('\n')
	}
	if len(c.Assumptions) > 0 {
		fmt.Fprintf(&b, "Assumed earlier (the person could not be asked): %s\n", strings.Join(c.Assumptions, "; "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderStageBlock renders <stage>: the stage number, whether the person can be
// asked, the ledger index and the earlier stages' results by reference.
func RenderStageBlock(s *domain.StagePlanning) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "stage: %d\n", s.Number)
	if s.CanAsk {
		b.WriteString("the person can be asked: yes\n")
	} else {
		b.WriteString("the person can be asked: no (do not use input_required; proceed and state your assumptions)\n")
	}
	if s.Lookup == nil {
		b.WriteString("ledger reads: not available on this call\n")
	}
	if strings.TrimSpace(s.EarlierStages) != "" {
		b.WriteString("\nEarlier stages' results (bind one in inputs by its e<K> label; workers read the entries with ledger_get):\n")
		b.WriteString(strings.TrimSpace(s.EarlierStages))
		b.WriteByte('\n')
	}
	if strings.TrimSpace(s.LedgerIndex) != "" {
		b.WriteString("\nLedger index for this request (id kind tool step: preview):\n")
		b.WriteString(strings.TrimSpace(s.LedgerIndex))
		b.WriteByte('\n')
	} else {
		b.WriteString("\nLedger index for this request: empty\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// serveLedgerReads runs the reads the planner asked for through the server's
// lookup (the ledger kernel tools under the caller's read predicate) and renders
// them for <ledger_reads>. Only ledger_get and ledger_find are served: the
// planner reads, it never acts.
func serveLedgerReads(ctx context.Context, s *domain.StagePlanning, reads []domain.LedgerLookup) string {
	if len(reads) == 0 {
		return "(the reply asked for reads but listed none)"
	}
	var b strings.Builder
	for _, r := range reads {
		args := strings.TrimSpace(string(r.Arguments))
		if args == "" {
			args = "{}"
		}
		fmt.Fprintf(&b, "%s %s ->\n", r.Tool, args)
		switch {
		case s.Lookup == nil:
			b.WriteString("not available: ledger reads are not served on this call\n")
		case r.Tool != "ledger_get" && r.Tool != "ledger_find":
			fmt.Fprintf(&b, "refused: only ledger_get and ledger_find can be read here, not %q\n", r.Tool)
		default:
			b.WriteString(strings.TrimSpace(s.Lookup(ctx, domain.LedgerLookup{Tool: r.Tool, Arguments: json.RawMessage(args)})))
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// normalizeQuestions numbers the questions Q1.., drops empty ones and maps an
// unknown kind to ambiguity (the kind is a hint for the person-facing wording,
// never a control decision).
func normalizeQuestions(qs []domain.InputQuestion) []domain.InputQuestion {
	var out []domain.InputQuestion
	for _, q := range qs {
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			continue
		}
		if !domain.ValidQuestionKind(q.Kind) {
			q.Kind = domain.QuestionAmbiguity
		}
		q.ID = fmt.Sprintf("Q%d", len(out)+1)
		out = append(out, q)
	}
	return out
}
