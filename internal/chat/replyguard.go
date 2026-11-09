package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/replycheck"
)

// The D8 reply check on the chat lane (ADR-0145; T-20261108-chat-replies-state-
// figures-dates-and-ids-no). Deterministic, no model call: the reply's figures,
// money amounts, dates and identifiers are compared with the turn's evidence (the
// ledger entries its delegated runs produced, the values the kernel accepted for
// each checklist item, the person's own words and today's date), and every
// checklist item must be answered or stated as a gap.
//
//   - observe: the report is logged, counted (ReplyCheckStats), recorded as a
//     computed ledger entry of the turn and emitted on the run trace as a
//     "reply_check" span the use-case harness collects. The reply is unchanged.
//   - enforce: a reply with violations gets ONE retry — the turn is dispatched
//     again with the uncited values and the unanswered items named and the
//     evidence attached, and the agent rewrites the reply in one call. What is
//     still wrong after it is stated plainly in a fixed sentence in the turn's
//     language (locale.go), never hidden.
//
// It runs kernel-side so it holds for whichever chat agent the pool runs, and it
// reads the evidence through the ENFORCED ledger reader. In enforce mode it
// replaces the chat agent's English-only "no access after a delegation" guard
// (agents/chat_agent.py _NO_ACCESS_CLAIM), which the agent then skips.

// Handoff metadata keys of the chat turn (read by agents/chat_agent.py).
const (
	turnKeyTurnIndex      = "turn_index"
	turnKeySummary        = "conversation_summary"
	turnKeyPendingInput   = "pending_input"
	turnKeyAskBack        = "ask_back"
	turnKeyPendingRequest = "pending_request"
	turnKeyGroundedReply  = "grounded_reply"
	turnKeyReplyCheck     = "reply_check"
	turnKeyDelegationRule = "delegation_rule"
	turnKeyLedgerTools    = "ledger_tools"
	turnKeyReplyRevision  = "reply_revision"
	turnKeyReplyEvidence  = "reply_evidence"
)

// replyRevision is the reply_revision payload of the one enforced retry.
type replyRevision struct {
	Draft   string   `json:"draft"`
	Uncited []string `json:"uncited,omitempty"`
	Gaps    []string `json:"gaps,omitempty"`
}

// turnEvidence gathers what the reply may rest on: the turn's citable ledger
// entries with their bodies, the checklist items of its runs, the person's words,
// today's date and the turn index the agent was shown (kernel records, not model
// text).
func (s *TurnService) turnEvidence(ctx context.Context, conv *domain.Conversation, userMsg domain.Message,
	history []domain.Message, results []domain.ExecuteResult, shownIndex string) replycheck.Evidence {
	var ev replycheck.Evidence
	for _, meta := range s.turnEntries(ctx, conv, userMsg.ID, s.Grounding.maxEntries()) {
		if meta.Kind == domain.LedgerScratch || strings.HasPrefix(meta.ToolName, "chat_") {
			continue
		}
		body := meta.Preview
		if meta.Kind != domain.LedgerStepOutput {
			if e, err := s.readBody(ctx, conv.OwnerID, meta.ID); err == nil && e != nil {
				body = string(e.Body)
			}
		}
		ev.Entries = append(ev.Entries, replycheck.Entry{ID: meta.ID, Kind: string(meta.Kind), Tool: meta.ToolName, Body: body})
	}
	for _, r := range results {
		for _, it := range indexItems(r.Requirements) {
			item := replycheck.Item{ID: it.ID, Run: r.RunID, Type: it.Type, Status: it.Status, Quote: it.Quote,
				Text: it.Text, Reason: it.Reason, Evidence: it.Evidence}
			for _, v := range it.Values {
				item.Values = append(item.Values, replycheck.Value{Name: v.Name, Type: v.Type, Text: v.Value, EvidenceRef: v.Ref})
			}
			ev.Items = append(ev.Items, item)
		}
	}
	ev.Context = append(ev.Context, userTexts(userMsg.Content, history)...)
	// Today, as the turn was told it (nil calendar ⇒ the wall clock in UTC).
	ev.Context = append(ev.Context, s.calendar.Block())
	if shownIndex != "" {
		ev.Context = append(ev.Context, shownIndex)
	}
	return ev
}

// groundReply runs the reply check (when on) and returns the reply the person
// sees, with the evidence markers stripped.
func (s *TurnService) groundReply(turnCtx, ctx context.Context, conv *domain.Conversation, userMsg domain.Message,
	history []domain.Message, h *domain.Handoff, reply string, results []domain.ExecuteResult, shownIndex, lang string) string {
	mode := s.Grounding.replyCheckMode()
	if mode == ReplyCheckOff || !s.ledgerOn() {
		return replycheck.StripMarkers(reply)
	}
	ev := s.turnEvidence(ctx, conv, userMsg, history, results, shownIndex)
	opt := replycheck.Options{MinInteger: s.Grounding.minInteger(), Lang: lang}
	rep := replycheck.Check(reply, ev, opt)
	s.observeReplyCheck(ctx, conv, userMsg, mode, 1, len(results) > 0, rep)
	if mode != ReplyCheckEnforce || rep.Violations() == 0 {
		return replycheck.StripMarkers(reply)
	}

	// Enforce: one retry naming what is wrong, over the same evidence.
	if revised, ok := s.reviseReply(turnCtx, conv, h, reply, rep, ev); ok {
		s.g.retries.Add(1)
		rep2 := replycheck.Check(revised, ev, opt)
		s.observeReplyCheck(ctx, conv, userMsg, mode, 2, len(results) > 0, rep2)
		reply, rep = revised, rep2
	}
	out := replycheck.StripMarkers(reply)
	if rep.Violations() > 0 {
		if note := gapStatement(rep, lang); note != "" {
			out += "\n\n" + note
		}
	}
	return out
}

// reviseReply dispatches the turn once more with the revision note and the
// evidence; the agent rewrites the reply in one call (agents/chat_agent.py
// reply_revision). ok is false when the retry could not run or returned nothing.
func (s *TurnService) reviseReply(turnCtx context.Context, conv *domain.Conversation, h *domain.Handoff, draft string,
	rep replycheck.Report, ev replycheck.Evidence) (string, bool) {
	rev := replyRevision{Draft: draft, Uncited: rep.UncitedRaw()}
	for _, g := range rep.Unanswered {
		label := g.Item
		if g.Quote != "" {
			label += " " + fmt.Sprintf("%q", g.Quote)
		}
		rev.Gaps = append(rev.Gaps, fmt.Sprintf("%s (%s, %s)", label, g.Status, g.Reason))
	}
	// A value stated from recalled memory names the document and article/section
	// it comes from (T-20261109-chat-agent-answers-a-published-rule).
	for _, m := range rep.MissingSource {
		rev.Gaps = append(rev.Gaps, fmt.Sprintf("%q comes from recalled memory [%s]: name its source (one of: %s)",
			m.Raw, m.Entry, strings.Join(m.Sources, "; ")))
	}
	b, err := json.Marshal(rev)
	if err != nil {
		return "", false
	}
	md := make(map[string]string, len(h.Payload.Metadata)+2)
	for k, v := range h.Payload.Metadata {
		md[k] = v
	}
	md[turnKeyReplyRevision] = string(b)
	md[turnKeyReplyEvidence] = renderEvidence(ev)
	retry := &domain.Handoff{FromAgent: h.FromAgent, Context: h.Context,
		Payload: &domain.Payload{Type: h.Payload.Type, Data: h.Payload.Data, Metadata: md}}
	resp, err := s.pool.Dispatch(turnCtx, retry)
	if err != nil || resp == nil || resp.Payload == nil {
		slog.Warn("chat: the reply check's retry did not run; keeping the first reply", "conversation", conv.ID, "err", err)
		return "", false
	}
	text := unwrapFinalAnswer(strings.TrimSpace(string(resp.Payload.Data)))
	if text == "" || looksLikeControlEnvelope(text) {
		return "", false
	}
	return text, true
}

// evidenceRenderEntries / evidenceRenderChars bound the evidence block the retry
// carries (the agent can ledger_get any entry by its id).
const (
	evidenceRenderEntries = 40
	evidenceRenderChars   = 600
)

// renderEvidence is the evidence the enforced retry rewrites the reply over: the
// checklist items with their status and accepted values, then the citable
// entries, each with its ledger id.
func renderEvidence(ev replycheck.Evidence) string {
	var b strings.Builder
	if len(ev.Items) > 0 {
		b.WriteString("Checklist (status decided by the kernel):\n")
		for _, it := range ev.Items {
			fmt.Fprintf(&b, "- [%s] %s %s: %s", it.ID, it.Type, it.Status, it.Text)
			if it.Quote != "" {
				fmt.Fprintf(&b, " (asked as %q)", it.Quote)
			}
			if it.Reason != "" && it.Status != "satisfied" {
				fmt.Fprintf(&b, " — %s", it.Reason)
			}
			for _, v := range it.Values {
				name := v.Name
				if name == "" {
					name = v.Type
				}
				fmt.Fprintf(&b, "\n    %s = %s", name, clip(v.Text, indexValueRunes))
				if v.EvidenceRef != "" {
					fmt.Fprintf(&b, " [%s]", v.EvidenceRef)
				}
			}
			b.WriteByte('\n')
		}
	}
	n := 0
	for _, e := range ev.Entries {
		if e.Kind == string(domain.LedgerStepOutput) || n >= evidenceRenderEntries {
			continue
		}
		n++
		if n == 1 {
			b.WriteString("Records read in this turn:\n")
		}
		fmt.Fprintf(&b, "- [%s] %s: %s\n", e.ID, e.Tool, clip(e.Body, evidenceRenderChars))
	}
	return strings.TrimRight(b.String(), "\n")
}

// gapStatement is the fixed sentence(s), in the turn's language, that state what
// the reply still does not back after the retry. Values are quoted as written and
// item quotes in the person's own words (data is never translated).
func gapStatement(rep replycheck.Report, lang string) string {
	var parts []string
	if vals := rep.UncitedRaw(); len(vals) > 0 {
		parts = append(parts, strings.ReplaceAll(Message(MsgReplyUnverified, lang), "{values}", strings.Join(vals, ", ")))
	}
	var items []string
	for _, g := range rep.Unanswered {
		q := strings.TrimSpace(g.Quote)
		if q == "" {
			continue
		}
		items = append(items, "“"+q+"”")
	}
	if len(items) > 0 {
		parts = append(parts, strings.ReplaceAll(Message(MsgReplyUncovered, lang), "{items}", strings.Join(items, ", ")))
	}
	// The sources a value from recalled memory rests on, named from the recall's
	// own ledger entry (the heading the value sits under first).
	var sources []string
	seen := map[string]bool{}
	for _, m := range rep.MissingSource {
		for i, src := range m.Sources {
			if i == maxSourcesNamed {
				break
			}
			if !seen[src] {
				seen[src] = true
				sources = append(sources, src)
			}
		}
	}
	if len(sources) > 0 {
		parts = append(parts, strings.ReplaceAll(Message(MsgReplySource, lang), "{sources}", strings.Join(sources, "; ")))
	}
	return strings.Join(parts, " ")
}

// maxSourcesNamed bounds the labels named per value in the source statement.
const maxSourcesNamed = 2

// observeReplyCheck logs, counts, records and traces one check. delegated says
// whether the turn delegated anything: a reply with uncited values from a turn that
// did not is the D3 measure (an organisation question answered without delegating).
func (s *TurnService) observeReplyCheck(ctx context.Context, conv *domain.Conversation, userMsg domain.Message,
	mode string, attempt int, delegated bool, rep replycheck.Report) {
	s.g.checkedTurns.Add(1)
	s.g.violations.Add(int64(rep.Violations()))
	level := slog.LevelInfo
	if rep.Violations() > 0 {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "chat: reply check", "event", "reply_check", "conversation", conv.ID, "turn", userMsg.ID,
		"mode", mode, "attempt", attempt, "delegated", delegated, "violations", rep.Violations(), "mentions", rep.Mentions,
		"uncited", rep.UncitedRaw(), "unanswered", len(rep.Unanswered), "miscited", len(rep.Miscited),
		"unverified_gaps", len(rep.Unverified), "unknown_markers", len(rep.UnknownMarkers),
		"evidence", rep.EvidenceCount, "items", rep.Items)
	record := struct {
		Mode      string            `json:"mode"`
		Attempt   int               `json:"attempt"`
		Delegated bool              `json:"delegated"`
		Report    replycheck.Report `json:"report"`
	}{mode, attempt, delegated, rep}
	s.recordState(ctx, conv, userMsg.ID, toolReplyCheck, record, nil)
	if s.g.tracer != nil {
		raw, _ := json.Marshal(rep)
		now := time.Now()
		s.g.tracer.Span(ctx, domain.TraceSpan{
			Name: "reply_check", Kind: "internal", Start: now, End: now,
			Attributes: map[string]any{
				domain.GenAIConversationID:        conv.ID,
				"cambrian.turn.id":                userMsg.ID,
				"cambrian.reply_check.mode":       mode,
				"cambrian.reply_check.attempt":    attempt,
				"cambrian.reply_check.delegated":  delegated,
				"cambrian.reply_check.violations": rep.Violations(),
				"cambrian.reply_check.uncited":    len(rep.Uncited),
				"cambrian.reply_check.unanswered": len(rep.Unanswered),
				"cambrian.reply_check.miscited":   len(rep.Miscited),
				"cambrian.reply_check.unverified": len(rep.Unverified),
				"cambrian.reply_check.mentions":   rep.Mentions,
				"cambrian.reply_check.evidence":   rep.EvidenceCount,
				"cambrian.reply_check.report":     string(raw),
			},
		})
	}
}
