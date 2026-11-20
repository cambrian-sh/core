package chat

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Interruption markers (ENG-2124, owner decision 2026-11-18): a turn that ends without a
// reply leaves one message of role "interruption" after the user's message, whose content
// is the plain sentence the person was told. On reopening the conversation the person sees
// that the turn did not complete and why — instead of a question silently never answered.
//
// The marker is the person's trace, never model context: history, the budgeted transcript
// and replay detection skip it (service.go, transcript.go).

// replayScan is how many messages after a replayed user message are read to find its reply
// past an interruption marker (a retried turn that failed, then succeeded).
const replayScan = 4

// unansweredSweepLimit bounds one boot sweep; a deployment with more turns cut by one
// stop than this has the rest marked at the next boot.
const unansweredSweepLimit = 1000

// interruptionMarkerKey is the marker's id and idempotency key: a natural key on the turn
// it marks (AGENTS.md §2.6), so the live path and the boot sweep, or a retried turn that
// fails again, never store a second marker for the same message.
func interruptionMarkerKey(userMessageID string) string {
	return "interrupt:" + userMessageID
}

// markInterrupted appends the interruption marker for the turn ordered by userMsg and
// reports whether it is stored. Best-effort: a turn that failed must still return its
// failure if the marker cannot be stored (a closed conversation, a store outage), so the
// error is logged, not returned.
func (s *TurnService) markInterrupted(ctx context.Context, userMsg domain.Message, text string) bool {
	if text == "" {
		text = Message(MsgGenericFail, s.DefaultLanguage)
	}
	key := interruptionMarkerKey(userMsg.ID)
	if _, err := s.store.AppendMessage(ctx, domain.Message{
		ID:             key,
		ConversationID: userMsg.ConversationID,
		Role:           domain.MessageRoleInterruption,
		Content:        text,
		ClientID:       key,
	}); err != nil {
		slog.Warn("chat: could not record the turn's interruption marker",
			"conversation", userMsg.ConversationID, "message", userMsg.ID, "err", err)
		return false
	}
	return true
}

// MarkInterruptedTurns marks the turns a kernel stop cut off: user messages created before
// `before` (the boot time — no turn of this process can be among them) that are still the
// last message of an open conversation get the "interrupted because the assistant
// restarted" marker, in the conversation's language. Returns how many were marked.
// Called once at boot by the composition root.
//
// A marked turn whose message arrived within RestartNoticeWindow before the boot is also
// delivered through the conversation's ingress (ENG-2146, owner decision 2026-11-20,
// option B): a person who asked over Telegram learns their question was dropped, and a
// long outage does not burst stale notices at everyone who ever asked. The marker is
// stored FIRST and is the source of truth, as for every emitted message; its id is the
// delivery's idempotency key, so a retried delivery is the same act. A conversation with
// no ingress (the console) has nowhere to push and is not a failure.
func (s *TurnService) MarkInterruptedTurns(ctx context.Context, before time.Time) (int, error) {
	turns, err := s.store.ListUnansweredTurns(ctx, before, unansweredSweepLimit)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, m := range turns {
		lang := s.turnLanguage(m.Content, nil)
		text := Message(interruptMessage(domain.TurnInterruptAssistantRestarted), lang)
		if !s.markInterrupted(ctx, m, text) || !s.restartNoticeDue(m, before) {
			continue
		}
		if s.deliverRestartNotice(ctx, m, text) {
			delivered++
		}
	}
	if delivered > 0 {
		slog.Info("chat: told people over their ingress that the last stop cut their turn off",
			"delivered", delivered, "window", s.RestartNoticeWindow)
	}
	if len(turns) == unansweredSweepLimit {
		slog.Warn("chat: more turns were cut off by the last stop than one boot sweep marks; the rest are marked at the next boot",
			"marked", len(turns))
	}
	return len(turns), nil
}

// restartNoticeDue reports whether the cut-off turn m is recent enough to be delivered:
// its message arrived at most RestartNoticeWindow before the boot. A message with no
// creation time is treated as old (never delivered): a stale notice is the failure the
// window exists to prevent.
func (s *TurnService) restartNoticeDue(m domain.Message, bootAt time.Time) bool {
	if s.deliverer == nil || s.RestartNoticeWindow <= 0 || m.CreatedAt.IsZero() {
		return false
	}
	return !m.CreatedAt.Before(bootAt.Add(-s.RestartNoticeWindow))
}

// deliverRestartNotice sends the stored marker text to the conversation's ingress, keyed by
// the marker id. Best-effort and logged: the transcript already holds the marker.
func (s *TurnService) deliverRestartNotice(ctx context.Context, m domain.Message, text string) bool {
	err := s.deliverer.Deliver(ctx, m.ConversationID, text, interruptionMarkerKey(m.ID))
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrNoDeliveryAddress):
		return false // a console conversation: nowhere to push
	default:
		slog.Warn("chat: could not tell the person over their ingress that the last stop cut their turn off; the transcript has the marker",
			"conversation", m.ConversationID, "message", m.ID, "err", err)
		return false
	}
}
