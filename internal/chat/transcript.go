package chat

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// The D2.1 transcript budget: with execution.chat.transcript_budget_tokens set,
// the verbatim transcript a turn carries is the newest messages that fit the
// budget (language, tone and pronouns live there), instead of the fixed
// HistoryLimit=40 message window; the messages that no longer fit are folded
// into the rolling summary (summary.go) by the kernel, not left to fall away.

// messageOverheadTokens is the per-message cost of the "person: " label and the
// line break in the rendered transcript.
const messageOverheadTokens = 4

// estimateTokens is the kernel's usual rough count (the spend meter's len/4
// fallback): budgeting needs a stable, cheap estimate, not a tokenizer. Bytes,
// not runes, so a Turkish text is over- rather than under-counted.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n == 0 {
		n = 1
	}
	return n
}

// budgetWindow splits msgs (oldest first) into the overflow that does not fit
// budget and the newest window that does. The newest message is always kept,
// whatever its size: a turn without the message it answers has no context.
func budgetWindow(msgs []domain.Message, budget int) (overflow, window []domain.Message) {
	used := 0
	k := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := estimateTokens(msgs[i].Content) + messageOverheadTokens
		if used+cost > budget && i < len(msgs)-1 {
			break
		}
		used += cost
		k = i
	}
	return msgs[:k], msgs[k:]
}

// loadBudgetedHistory is loadHistory under the token budget: the messages after
// the conversation's latest summary, cut to the budget, and the summary with any
// overflow folded in. Without the ledger there is no summary to keep, so the
// overflow is simply out of view (the wiring warns).
func (s *TurnService) loadBudgetedHistory(ctx context.Context, conv *domain.Conversation, upToSeq int64, turnID string) ([]domain.Message, string, error) {
	prev := conversationSummary{}
	if s.ledgerOn() {
		prev = s.latestSummary(ctx, conv)
	}
	msgs, err := s.store.ListMessages(ctx, conv.ID, prev.ThroughSeq, 0)
	if err != nil {
		return nil, "", err
	}
	all := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		// Interruption markers are the person's trace, not model context (ENG-2124).
		if m.Seq < upToSeq && m.Role != domain.MessageRoleInterruption {
			all = append(all, m)
		}
	}
	var overflow, window []domain.Message
	if s.Lane.SingleAgent {
		// ADR-0150 D3: older long replies cleared first (no LLM); only what still
		// does not fit is summarised.
		overflow, window = compactLane(all, s.Grounding.TranscriptBudgetTokens)
	} else {
		overflow, window = budgetWindow(all, s.Grounding.TranscriptBudgetTokens)
	}
	if len(overflow) > 0 && s.ledgerOn() {
		prev = s.foldSummary(ctx, conv, turnID, prev, overflow)
	}
	if s.Lane.SingleAgent && prev.ThroughSeq > 0 {
		// The person's own words survive the summary verbatim, up to the cap.
		if kept := s.keptUserMessages(ctx, conv, prev.ThroughSeq, s.Lane.KeepUserMessagesTokens); len(kept) > 0 {
			window = append(kept, window...)
		}
	}
	return window, prev.Text, nil
}
