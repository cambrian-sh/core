package chat

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/tokenest"
)

// ADR-0150 D3: in the single-agent lane (execution.lane.default single_agent) the
// agent the person talks to IS the default-lane agent. There is no separate front
// desk that delegates to the planner: the turn is the run, the agent holds the
// person's permitted tools (every call still through ExecuteTool with the turn's
// lease: grants, approvals, the conversation owner as run principal, the ledger,
// the trace, turn liveness) and answers itself. delegate_to_planner is not
// offered, and the kernel refuses it (network.singleAgentDelegationRefusal).

// LaneTurn is the chat side of the single-agent lane; the zero value is the
// planner lane's front desk, unchanged (ADR-0150 D8).
type LaneTurn struct {
	// SingleAgent turns the lane on for chat turns.
	SingleAgent bool
	// Limits are the D4 caps and stuck thresholds stamped on every turn (the
	// agent loop enforces them; network.Server.LaneLimits).
	Limits domain.LaneLimits
	// KeepUserMessagesTokens is execution.chat.keep_user_messages_tokens: the
	// person's own messages that left the transcript budget are still carried
	// verbatim, newest first, up to this many estimated tokens. 0 keeps none.
	KeepUserMessagesTokens int
}

// TurnComposer binds the person's permitted tools to a turn's lease (the
// composition root closes over network.Server.ComposeLease: one composition of
// the request, scope-checked against the conversation owner, enforced by the
// tool executor's lease ceiling). An error leaves the turn on the chat agent's
// own grants (logged).
type TurnComposer func(ctx context.Context, lease domain.LeaseID, brief string) error

// SetTurnComposer wires the D3 tool binding of a single-agent turn.
func (s *TurnService) SetTurnComposer(f TurnComposer) { s.composeTurn = f }

// D3 pre-pass sizes: an older agent reply longer than clearReplyChars is
// shortened to its head before any summary call — a cheap, no-LLM clearing
// (Hermes' pre-pass, Anthropic's "tool-result clearing"): in this lane long
// replies are where tool output reached the transcript, and the values behind
// them are in the run ledger.
const (
	clearReplyChars = 1200
	clearReplyHead  = 300
	// clearKeepNewest is how many of the newest messages are never shortened.
	clearKeepNewest = 4
)

// clearedReplyNote ends a shortened reply; it says where the rest is.
const clearedReplyNote = " … [earlier reply shortened in this transcript; the values it used are in the conversation's ledger: ledger_find scope=conversation]"

// clearOldReplies returns msgs (oldest first) with every agent reply older than
// the newest clearKeepNewest messages and longer than clearReplyChars cut to
// its head plus clearedReplyNote. The person's messages are never touched. The
// store is unchanged: this is what the turn carries, not what is kept.
func clearOldReplies(msgs []domain.Message) []domain.Message {
	out := make([]domain.Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < len(out)-clearKeepNewest; i++ {
		m := out[i]
		if m.Role != domain.MessageRoleAgent || len(m.Content) <= clearReplyChars {
			continue
		}
		head := m.Content[:clearReplyHead]
		// Cut on a rune boundary.
		for len(head) > 0 && !utf8Start(m.Content[len(head)]) {
			head = head[:len(head)-1]
		}
		m.Content = strings.TrimSpace(head) + clearedReplyNote
		out[i] = m
	}
	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// keptUserMessages are the person's own messages at or before throughSeq (the
// ones the summary now covers), newest first up to budget tokens, returned
// oldest first (ADR-0150 D3: "keeping the person's own messages verbatim up to a
// cap", Codex's 20k).
func (s *TurnService) keptUserMessages(ctx context.Context, conv *domain.Conversation, throughSeq int64, budget int) []domain.Message {
	if budget <= 0 || throughSeq <= 0 {
		return nil
	}
	msgs, err := s.store.ListMessages(ctx, conv.ID, 0, 0)
	if err != nil {
		slog.Warn("chat: could not load the person's earlier messages", "conversation", conv.ID, "err", err)
		return nil
	}
	var kept []domain.Message
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Seq > throughSeq || m.Role != domain.MessageRoleUser {
			continue
		}
		cost := tokenest.Estimate(m.Content) + messageOverheadTokens
		if used+cost > budget {
			break
		}
		used += cost
		kept = append(kept, m)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// compactLane is the single-agent lane's cross-turn compaction over the
// messages after the latest summary (all, oldest first): first the no-LLM
// pre-pass (clearOldReplies), and only what still does not fit goes to the
// summary call (the caller folds overflow, original wording, into the
// structured summary). Returns the window to carry and the overflow to fold.
func compactLane(all []domain.Message, budget int) (overflow, window []domain.Message) {
	overflow, window = budgetWindow(all, budget)
	if len(overflow) == 0 {
		return nil, window
	}
	cleared := clearOldReplies(all)
	ovf, win := budgetWindow(cleared, budget)
	// The summary folds the ORIGINAL messages: the pre-pass only shortens what
	// the turn carries.
	return all[:len(ovf)], win
}
