package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// The D2.2 turn index and the D5a pending ask-back state.
//
// Each delegated turn leaves ONE index entry, built by the kernel from the
// structured results of the runs the turn delegated (TurnDelegations captured
// them from Execute; the agent relays nothing): the run ids, each checklist item
// with its code-decided status and its accepted values by name with the ledger
// entry each came from, the actions with the ids the systems returned (numbered
// per tool, so "the second draft" is the second mail_draft), and the completed
// steps' ledger refs. Later turns are shown a compact rendering of the recent
// entries, so a follow-up resolves by reference instead of by re-reading prose.

// turnIndexVersion is the shape version of TurnIndexEntry.
const turnIndexVersion = 1

// TurnIndexEntry is one delegated turn's record.
type TurnIndexEntry struct {
	Version int    `json:"version"`
	TurnID  string `json:"turn_id"`
	Seq     int64  `json:"seq"`
	// Request is the person's message that opened the turn, bounded.
	Request string     `json:"request"`
	Runs    []IndexRun `json:"runs"`
	// Pending is set when the turn's last run stopped to ask the person (D5a).
	Pending *domain.InputRequired `json:"pending,omitempty"`
}

// IndexRun is one delegated run of the turn.
type IndexRun struct {
	RunID    string        `json:"run_id,omitempty"`
	Partial  bool          `json:"partial,omitempty"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Items    []IndexItem   `json:"items,omitempty"`
	Actions  []IndexAction `json:"actions,omitempty"`
	Steps    []IndexStep   `json:"steps,omitempty"`
}

// IndexItem is one checklist item with its kernel-decided status.
type IndexItem struct {
	ID     string       `json:"id"`
	Type   string       `json:"type"`
	Text   string       `json:"text"`
	Quote  string       `json:"quote,omitempty"`
	Status string       `json:"status"`
	Reason string       `json:"reason,omitempty"`
	Values []IndexValue `json:"values,omitempty"`
	// Evidence are the ledger ids behind the accepted values.
	Evidence []string `json:"evidence,omitempty"`
}

// IndexValue is one accepted value with its ledger reference.
type IndexValue struct {
	Name  string `json:"name,omitempty"`
	Type  string `json:"type"`
	Value string `json:"value"`
	Ref   string `json:"ref,omitempty"`
}

// IndexAction is one side-effecting call of the run; N numbers the calls of the
// same tool in order (the second mail_draft is N=2).
type IndexAction struct {
	N      int      `json:"n"`
	Tool   string   `json:"tool"`
	Status string   `json:"status"`
	IDs    []string `json:"ids,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// IndexStep is a completed step's ledger reference.
type IndexStep struct {
	Step    int    `json:"step"`
	Stage   int    `json:"stage,omitempty"`
	Ref     string `json:"ref"`
	Preview string `json:"preview,omitempty"`
}

// Bounds of what an index entry keeps: the index is a pointer into the ledger,
// not a copy of it (values stay readable with ledger_get by their refs).
const (
	indexRequestRunes = 300
	indexValueRunes   = 160
	indexPreviewRunes = 120
	indexMaxSteps     = 12
)

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// coverageJSON is the shape of ExecuteResult.Requirements (network.recordCoverage:
// the checklist plus requirements.Report).
type coverageJSON struct {
	Items []struct {
		Item struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Text  string `json:"text"`
			Quote string `json:"quote"`
		} `json:"item"`
		Status string `json:"status"`
		Reason string `json:"reason"`
		Values []struct {
			Name        string          `json:"name"`
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			EvidenceRef string          `json:"evidence_ref"`
		} `json:"values"`
		Evidence []string `json:"evidence"`
	} `json:"items"`
}

func valueText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// indexItems decodes a run's coverage into index items.
func indexItems(raw json.RawMessage) []IndexItem {
	if len(raw) == 0 {
		return nil
	}
	var cov coverageJSON
	if err := json.Unmarshal(raw, &cov); err != nil {
		slog.Warn("chat: run coverage not decodable for the turn index", "err", err)
		return nil
	}
	var out []IndexItem
	for _, it := range cov.Items {
		ii := IndexItem{ID: it.Item.ID, Type: it.Item.Type, Text: it.Item.Text, Quote: it.Item.Quote,
			Status: it.Status, Reason: it.Reason, Evidence: it.Evidence}
		for _, v := range it.Values {
			ii.Values = append(ii.Values, IndexValue{Name: v.Name, Type: v.Type, Value: valueText(v.Value), Ref: v.EvidenceRef})
		}
		out = append(out, ii)
	}
	return out
}

// buildTurnIndex builds the turn's entry from its delegated runs' results.
func buildTurnIndex(turnID string, seq int64, request string, results []domain.ExecuteResult) TurnIndexEntry {
	e := TurnIndexEntry{Version: turnIndexVersion, TurnID: turnID, Seq: seq, Request: clip(request, indexRequestRunes)}
	for _, r := range results {
		run := IndexRun{RunID: r.RunID, Partial: r.Partial, TimedOut: r.TimedOut, Items: indexItems(r.Requirements)}
		perTool := map[string]int{}
		for _, a := range r.Actions {
			perTool[a.Tool]++
			run.Actions = append(run.Actions, IndexAction{N: perTool[a.Tool], Tool: a.Tool, Status: a.Status,
				IDs: a.IDs, Detail: clip(a.Detail, indexValueRunes)})
		}
		for _, st := range r.Steps {
			if st.Ref == "" || st.Status != domain.ExecuteStepCompleted || len(run.Steps) >= indexMaxSteps {
				continue
			}
			run.Steps = append(run.Steps, IndexStep{Step: st.Step, Stage: st.Stage, Ref: st.Ref, Preview: clip(st.Preview, indexPreviewRunes)})
		}
		e.Runs = append(e.Runs, run)
	}
	if n := len(results); n > 0 && results[n-1].InputRequired != nil {
		e.Pending = results[n-1].InputRequired
	}
	return e
}

// refs lists every ledger id the entry points at (its DerivedFrom).
func (e TurnIndexEntry) refs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if domain.IsLedgerID(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, r := range e.Runs {
		for _, it := range r.Items {
			for _, v := range it.Values {
				add(v.Ref)
			}
			for _, id := range it.Evidence {
				add(id)
			}
		}
		for _, st := range r.Steps {
			add(st.Ref)
		}
	}
	if e.Pending != nil {
		add(e.Pending.LedgerRef)
	}
	return out
}

// renderTurnIndex is the compact index the chat agent is shown, oldest first.
// Values keep their language (data is never translated); the labels are English
// (internal language).
func renderTurnIndex(entries []TurnIndexEntry) string {
	var b strings.Builder
	for i, e := range entries {
		label := fmt.Sprintf("T%d", i+1)
		if i == len(entries)-1 {
			label += " (latest)"
		}
		fmt.Fprintf(&b, "%s turn %s: %q\n", label, e.TurnID, e.Request)
		for _, r := range e.Runs {
			state := ""
			switch {
			case r.TimedOut:
				state = ", stopped by a time limit"
			case r.Partial:
				state = ", partial"
			}
			fmt.Fprintf(&b, "  run %s%s\n", r.RunID, state)
			for _, it := range r.Items {
				fmt.Fprintf(&b, "    %s %s [%s] %s", it.ID, it.Type, it.Status, it.Text)
				if it.Reason != "" && it.Status != "satisfied" {
					fmt.Fprintf(&b, " (%s)", clip(it.Reason, indexValueRunes))
				}
				var vals []string
				for _, v := range it.Values {
					name := v.Name
					if name == "" {
						name = v.Type
					}
					s := name + "=" + clip(v.Value, indexValueRunes)
					if v.Ref != "" {
						s += " " + v.Ref
					}
					vals = append(vals, s)
				}
				if len(vals) > 0 {
					b.WriteString(": " + strings.Join(vals, "; "))
				}
				b.WriteByte('\n')
			}
			for _, a := range r.Actions {
				fmt.Fprintf(&b, "    action %s #%d %s", a.Tool, a.N, a.Status)
				if len(a.IDs) > 0 {
					b.WriteString(" ids=" + strings.Join(a.IDs, ","))
				}
				b.WriteByte('\n')
			}
			for _, st := range r.Steps {
				fmt.Fprintf(&b, "    step %d %s: %s\n", st.Step, st.Ref, st.Preview)
			}
		}
		if e.Pending != nil {
			b.WriteString("  asked the person (awaiting the answer):\n")
			for _, q := range e.Pending.Questions {
				fmt.Fprintf(&b, "    %s %s\n", q.ID, q.Question)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// loadTurnIndex returns the conversation's index entries, oldest first, at most
// n (the most recent).
// refs is the union of the loaded entries' source refs: what a turn shown them may
// repeat (ENG-2155).
func (s *TurnService) loadTurnIndex(ctx context.Context, conv *domain.Conversation, n int) (_ []TurnIndexEntry, refs []string) {
	list := s.findState(ctx, conv, toolTurnIndex)
	if len(list) > n {
		list = list[len(list)-n:]
	}
	var out []TurnIndexEntry
	var sets [][]string
	for _, meta := range list {
		e, err := s.readBody(ctx, conv.OwnerID, meta.ID)
		if err != nil || e == nil {
			continue
		}
		var te TurnIndexEntry
		if json.Unmarshal(e.Body, &te) != nil {
			continue
		}
		out = append(out, te)
		sets = append(sets, e.SourceRefs)
	}
	return out, unionRefs(sets...)
}

// turnContext is what the phase-3 keys add to a turn before it is dispatched.
type turnContext struct {
	index   []TurnIndexEntry
	summary string
	// pending is the previous turn's open ask-back, when this message answers it.
	pending *domain.InputRequired
	// pendingRef is the ledger entry holding it (InputRequired.LedgerRef).
	pendingRef string
	// pendingRequest is the person's message that the ask-back was about.
	pendingRequest string
	// shownIndex is the rendered index the agent was shown (kernel records; the
	// reply check accepts values restated from it).
	shownIndex string
	// given is everything the turn was GIVEN besides its own recalls, with the memory
	// records behind each: the transcript's replies, the rolling summary and the turn
	// index entries it was shown (ENG-2155). A reply that repeats any of it records
	// those records, so forgetting one of them redacts the reply too — but only what
	// the reply can have repeated travels (ENG-2165, givenSource).
	given []givenSource
}

// loadTurnContext fills tc with the turn index and an open ask-back, per the
// Grounding keys (the summary was set with the history).
func (s *TurnService) loadTurnContext(ctx context.Context, conv *domain.Conversation, history []domain.Message, tc *turnContext) {
	if !s.ledgerOn() || !(s.Grounding.TurnIndex || s.Grounding.AskBack) {
		return
	}
	index, indexRefs := s.loadTurnIndex(ctx, conv, s.Grounding.indexTurns())
	if s.Grounding.AskBack {
		tc.pending, tc.pendingRef = pendingFor(index, history)
		if tc.pending != nil {
			tc.pendingRequest = index[len(index)-1].Request
		}
	}
	if s.Grounding.TurnIndex {
		tc.index = index
		tc.shownIndex = renderTurnIndex(index)
		tc.given = append(tc.given, givenSource{text: tc.shownIndex, onOverlap: indexRefs})
	} else if tc.pending != nil {
		// Only the open ask-back was shown, from the latest entry; its refs are in
		// indexRefs with the rest. Broad on purpose: a superset never misses a value.
		tc.given = append(tc.given, givenSource{text: renderTurnIndex(index), onOverlap: indexRefs})
	}
}

// addTurnContext puts the phase-3 keys on the turn handoff
// (agents/chat_agent.py reads them).
func (s *TurnService) addTurnContext(h *domain.Handoff, tc *turnContext) {
	if h == nil || h.Payload == nil {
		return
	}
	md := h.Payload.Metadata
	set := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	set(turnKeySummary, tc.summary)
	set(turnKeyTurnIndex, tc.shownIndex)
	if tc.pending != nil {
		set(turnKeyPendingInput, renderPending(tc.pending, tc.pendingRequest))
		// The pending request's own words, verbatim: the agent's delegation carries them
		// with the answer, so the checklist comes from the person's words (D3/D5a).
		set(turnKeyPendingRequest, tc.pendingRequest)
	}
	g := s.Grounding
	if g.GroundedReply {
		md[turnKeyGroundedReply] = "true"
	}
	if mode := g.replyCheckMode(); mode != ReplyCheckOff {
		md[turnKeyReplyCheck] = mode
	}
	if g.DelegationRule {
		md[turnKeyDelegationRule] = "true"
	}
	if g.AskBack {
		md[turnKeyAskBack] = "true"
	}
	if g.LedgerTools && s.ledgerOn() {
		md[turnKeyLedgerTools] = "true"
	}
}

// pendingFor returns the ask-back the previous turn left open, when the latest
// index entry belongs to the message right before this one (an index entry from
// an older turn has been answered or abandoned since).
func pendingFor(index []TurnIndexEntry, history []domain.Message) (*domain.InputRequired, string) {
	if len(index) == 0 {
		return nil, ""
	}
	last := index[len(index)-1]
	if last.Pending == nil {
		return nil, ""
	}
	prevUser := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == domain.MessageRoleUser {
			prevUser = history[i].ID
			break
		}
	}
	if prevUser == "" || prevUser != last.TurnID {
		return nil, ""
	}
	return last.Pending, last.Pending.LedgerRef
}

// renderPending is the open ask-back the chat agent resumes from: the original
// request (verbatim) and the questions it was asked.
func renderPending(p *domain.InputRequired, request string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "round %d", p.Round)
	if p.RunID != "" {
		fmt.Fprintf(&b, ", run %s", p.RunID)
	}
	b.WriteString("\nThe earlier request:\n" + request + "\nThe questions asked:\n")
	for _, q := range p.Questions {
		fmt.Fprintf(&b, "- %s %s", q.ID, q.Question)
		if len(q.Options) > 0 {
			b.WriteString(" (options: " + strings.Join(q.Options, " | ") + ")")
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// recordTurnIndex writes the turn's index entry when it delegated anything. refs are
// the reply's sources: the entry restates the turn's runs (an action's detail even
// when its step left no output for DerivedFrom to name), so an erase of any record
// the turn rested on deletes it with the reply's redaction (ENG-2155).
func (s *TurnService) recordTurnIndex(ctx context.Context, conv *domain.Conversation, userMsg domain.Message, results []domain.ExecuteResult, refs []string) {
	if len(results) == 0 || !(s.Grounding.TurnIndex || s.Grounding.AskBack) {
		return
	}
	e := buildTurnIndex(userMsg.ID, userMsg.Seq, userMsg.Content, results)
	if !s.Grounding.AskBack {
		e.Pending = nil
	}
	s.recordState(ctx, conv, userMsg.ID, toolTurnIndex, e, e.refs(), refs)
}
