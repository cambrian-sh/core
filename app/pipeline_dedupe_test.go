package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// One entry organ, one row.
//
// The regression: a deployment with TWO Telegram bots listed FIVE pipelines.
// Four were chat pipelines (two for bots that had been deleted), and the fifth
// was the same bot again under its migrated-watch id. Keying the dedupe on the
// pipeline id treated `telegram_bot` and `ingress:telegram_bot` as two things.

func row(id, triggerType, ref string, revision int) domain.PipelineSummary {
	return domain.PipelineSummary{
		PipelineID: id, TriggerType: triggerType, TriggerRef: ref, Revision: revision,
	}
}

func ids(rows []domain.PipelineSummary) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.PipelineID)
	}
	return out
}

func TestPreferAuthored_CollapsesOneIngressDescribedTwice(t *testing.T) {
	got := preferAuthored([]domain.PipelineSummary{
		// The migrated watch, keyed on the agent id as its stream.
		row("telegram_ingress_agent", "stream", "telegram_ingress_agent", 1),
		// The chat pipeline for the SAME bot.
		row("ingress:telegram_ingress_agent", "ingress", "telegram_ingress_agent", 1),
	})
	if len(got) != 1 {
		t.Fatalf("one bot must be one row, got %v", ids(got))
	}
	// The chat pipeline wins: it is the thing that actually handles the turn,
	// and the migrated watch is the shape it used to have.
	if got[0].PipelineID != "ingress:telegram_ingress_agent" {
		t.Fatalf("the ingress-triggered row must win, got %q", got[0].PipelineID)
	}
}

func TestPreferAuthored_KeepsGenuinelyDifferentEntryPoints(t *testing.T) {
	got := preferAuthored([]domain.PipelineSummary{
		row("ingress:bot_a", "ingress", "bot_a", 1),
		row("ingress:bot_b", "ingress", "bot_b", 1),
	})
	if len(got) != 2 {
		t.Fatalf("two bots are two rows, got %v", ids(got))
	}
}

// An authored revision beats a derived description of the same entry point.
func TestPreferAuthored_AuthoredBeatsDerived(t *testing.T) {
	got := preferAuthored([]domain.PipelineSummary{
		row("ingress:bot", "ingress", "bot", 0), // derived: nothing authored
		row("ingress:bot", "ingress", "bot", 3), // authored
	})
	if len(got) != 1 || got[0].Revision != 3 {
		t.Fatalf("the authored revision must win, got %+v", got)
	}
}

func TestPreferAuthored_LaterRevisionWins(t *testing.T) {
	got := preferAuthored([]domain.PipelineSummary{
		row("ingress:bot", "ingress", "bot", 2),
		row("ingress:bot", "ingress", "bot", 5),
	})
	if len(got) != 1 || got[0].Revision != 5 {
		t.Fatalf("expected revision 5, got %+v", got)
	}
}

// A row with no trigger reference falls back to its own id, so two unbound
// pipelines do not collapse into one.
func TestPreferAuthored_UnboundRowsDoNotCollapse(t *testing.T) {
	got := preferAuthored([]domain.PipelineSummary{
		row("a", "manual", "", 1),
		row("b", "manual", "", 1),
	})
	if len(got) != 2 {
		t.Fatalf("two unbound pipelines are two rows, got %v", ids(got))
	}
}
