package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// The D2.3 rolling summary: when the transcript a turn carries exceeds
// execution.chat.transcript_budget_tokens, the KERNEL condenses the messages that
// no longer fit into the conversation's running summary with one organ call.
// The summary may lose wording but not data: values stay in the ledger, reachable
// through the turn index and ledger_find. It is kept as a computed ledger entry
// recording the last message it covers, so each message is summarised once.

var summaryPrompts = prompts.MustPair("conversation_summary", "conversation_summary_input")

var summaryDataTags = []string{"previous", "messages"}

func init() {
	h := summaryPrompts.StaticHash()
	domain.PromptRegistry[h] = domain.PromptEntry{
		ID: "chat.conversation_summary",
		Version: fmt.Sprintf("%s.v%d+%s.v%d", summaryPrompts.System.ID, summaryPrompts.System.Version,
			summaryPrompts.Input.ID, summaryPrompts.Input.Version),
		Hash: h,
	}
}

// NewSummarizer returns the Summarizer that calls gen (a kernel organ generator).
func NewSummarizer(gen domain.Generator) Summarizer {
	if gen == nil {
		return nil
	}
	return func(ctx context.Context, previous string, msgs []domain.Message) (string, error) {
		values := map[string]string{"previous": previous, "messages": summaryLines(msgs)}
		for k, v := range values {
			values[k] = prompts.NeutraliseClosingTags(strings.TrimRight(v, "\n"), summaryDataTags...)
		}
		msg, err := summaryPrompts.Message(values)
		if err != nil {
			return "", fmt.Errorf("summary prompt: %w", err)
		}
		resp, err := domain.GenerateVia(ctx, gen, domain.GenerateRequest{
			System:   []domain.SystemBlock{{Text: summaryPrompts.System.Body, Cache: true}},
			Messages: []domain.ModelMessage{domain.UserMessage(msg)},
			CacheTTL: domain.CacheTTLLong,
		})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(resp.Text), nil
	}
}

func summaryLines(msgs []domain.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case domain.MessageRoleAgent:
			b.WriteString("assistant: ")
		case domain.MessageRoleSystem:
			b.WriteString("system: ")
		default:
			b.WriteString("person: ")
		}
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteByte('\n')
	}
	return b.String()
}

// conversationSummary is the summary entry's body.
type conversationSummary struct {
	// ThroughSeq is the last message the summary covers.
	ThroughSeq int64  `json:"through_seq"`
	Text       string `json:"text"`
}

// latestSummary returns the conversation's newest summary, or the zero value.
func (s *TurnService) latestSummary(ctx context.Context, conv *domain.Conversation) conversationSummary {
	list := s.findState(ctx, conv, toolSummary)
	for i := len(list) - 1; i >= 0; i-- {
		e, err := s.readBody(ctx, conv.OwnerID, list[i].ID)
		if err != nil || e == nil {
			continue
		}
		var cs conversationSummary
		if json.Unmarshal(e.Body, &cs) == nil {
			return cs
		}
	}
	return conversationSummary{}
}

// foldSummary folds the overflow messages into prev and records the result
// against the turn. On failure the previous summary stands (logged): the turn
// still runs, with the overflow out of view.
func (s *TurnService) foldSummary(ctx context.Context, conv *domain.Conversation, turnID string, prev conversationSummary, overflow []domain.Message) conversationSummary {
	if len(overflow) == 0 || s.g.summarize == nil {
		return prev
	}
	text, err := s.g.summarize(ctx, prev.Text, overflow)
	if err != nil || strings.TrimSpace(text) == "" {
		slog.Warn("chat: the rolling summary call failed; older messages are out of the transcript without a summary",
			"conversation", conv.ID, "messages", len(overflow), "err", err)
		return prev
	}
	next := conversationSummary{ThroughSeq: overflow[len(overflow)-1].Seq, Text: text}
	s.recordState(ctx, conv, turnID, toolSummary, next, nil)
	slog.Info("chat: rolling summary updated", "conversation", conv.ID, "through_seq", next.ThroughSeq,
		"folded", len(overflow))
	return next
}
