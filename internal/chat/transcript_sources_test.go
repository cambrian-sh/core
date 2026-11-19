package chat

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2155 (owner decision 2026-11-19, the broad rule): everything a turn was given —
// the transcript's replies, the rolling summary, the turn index — and everything the
// turn emits carries the memory records behind it, so forgetting one of them reaches
// every copy of its value.

// redact simulates the erase's reply redaction (PgErasureStore.redactReplies) on the
// in-memory store.
func (r *groundingRig) redact(id string) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	now := time.Now()
	for i, m := range r.store.msgs["c1"] {
		if m.ID == id {
			r.store.msgs["c1"][i].Content = domain.RedactedReplyText
			r.store.msgs["c1"][i].RedactedAt = &now
		}
	}
}

// redactCiting redacts every agent reply resting on ref, as redactReplies does
// (role agent, source_refs ?| refs).
func (r *groundingRig) redactCiting(ref string) (ids []string) {
	msgs, _ := r.store.ListMessages(context.Background(), "c1", -1, 0)
	for _, m := range msgs {
		if m.Role != domain.MessageRoleAgent || m.RedactedAt != nil {
			continue
		}
		for _, x := range m.SourceRefs {
			if x == ref {
				r.redact(m.ID)
				ids = append(ids, m.ID)
				break
			}
		}
	}
	return ids
}

// eraseLedger simulates the erase's ledger step (PgErasureStore.eraseLedger): every
// entry whose source refs meet ref is deleted.
func (r *groundingRig) eraseLedger(ref string) (deleted []string) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	kept := r.ledger.entries[:0]
	for _, e := range r.ledger.entries {
		hit := false
		for _, x := range e.SourceRefs {
			hit = hit || x == ref
		}
		if hit {
			deleted = append(deleted, e.ToolName)
			continue
		}
		kept = append(kept, e)
	}
	r.ledger.entries = kept
	return deleted
}

func TestARepeatReplyInheritsTheSourcesOfTheTranscriptItWasGiven(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.recall(r.lastUserID(), "chunk-1", "doc-1")
		}
	}
	r.pool.replies = []string{"The agreement number is HERON-7.", "As I said: HERON-7."}
	first, _ := r.turn(t, "What is the agreement number?")
	repeat, _ := r.turn(t, "Say it again?") // recalls nothing; repeats from the transcript
	want := []string{"chunk-1", "doc-1"}
	if !reflect.DeepEqual(first.SourceRefs, want) || !reflect.DeepEqual(repeat.SourceRefs, want) {
		t.Fatalf("refs: first %v, repeat %v; want both %v", first.SourceRefs, repeat.SourceRefs, want)
	}
}

// A reply the erase already redacted holds no value any more: its refs do not travel
// into the replies of later turns.
func TestARedactedReplysSourcesDoNotTravel(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 1 {
			r.recall(r.lastUserID(), "doc-1")
		}
	}
	first, _ := r.turn(t, "q1")
	r.redact(first.ID)
	r.eraseLedger("doc-1")
	later, _ := r.turn(t, "q2")
	if len(later.SourceRefs) != 0 {
		t.Fatalf("a reply given only a redacted reply rests on %v", later.SourceRefs)
	}
}

func TestTheRollingSummaryRecordsTheSourcesOfWhatItCondensed(t *testing.T) {
	r := newGroundingRig(t, Grounding{TranscriptBudgetTokens: 60})
	var folded []string
	r.svc.SetSummarizer(func(_ context.Context, prev string, msgs []domain.Message) (string, error) {
		text := prev
		for _, m := range msgs {
			text += " | " + m.Content
		}
		folded = append(folded, text)
		return text, nil
	})
	long := strings.Repeat("word ", 40)
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		if call == 2 {
			r.recall(r.lastUserID(), "chunk-1", "doc-1")
		}
	}
	r.pool.replies = []string{long, "HERON-7 " + long, long, long, long, "We said HERON-7."}
	r.turn(t, "pad "+long)
	quoting, _ := r.turn(t, "the agreement number?")
	r.turn(t, "next")
	r.turn(t, "and next") // the quoting reply is folded into the summary by now
	sums := r.ledger.tool(toolSummary)
	var covering int
	for _, e := range sums {
		var hasDoc bool
		for _, ref := range e.SourceRefs {
			hasDoc = hasDoc || ref == "doc-1"
		}
		if strings.Contains(string(e.Body), "HERON-7") {
			covering++
			if !hasDoc {
				t.Errorf("a summary quoting the reply records refs %v, want doc-1 among them", e.SourceRefs)
			}
		}
	}
	if covering == 0 {
		t.Fatalf("precondition: no summary condensed the quoting reply (%d summaries)", len(sums))
	}

	// ENG-2165: a turn given that summary that repeats nothing of it does not rest on
	// it (option B: a summary's records travel only on a shared value)...
	unrelated, h := r.turn(t, "something else")
	if !strings.Contains(h.Payload.Metadata[turnKeySummary], "HERON-7") {
		t.Fatalf("precondition: the unrelated turn was given the summary: %q", h.Payload.Metadata[turnKeySummary])
	}
	if len(unrelated.SourceRefs) != 0 {
		t.Fatalf("an unrelated reply given the summary rests on %v, want nothing", unrelated.SourceRefs)
	}
	// ... and a turn that copies the value from it rests on it.
	given, h := r.turn(t, "what did we say?")
	if !strings.Contains(h.Payload.Metadata[turnKeySummary], "HERON-7") {
		t.Fatalf("precondition: the turn was given the summary: %q", h.Payload.Metadata[turnKeySummary])
	}
	if !reflect.DeepEqual(given.SourceRefs, []string{"chunk-1", "doc-1"}) {
		t.Fatalf("a reply given the summary rests on %v, want the quoted document", given.SourceRefs)
	}

	// The forget: the erase deletes every summary that condensed the value and redacts
	// the replies; the next turn folds the redacted transcript again and is never given
	// the value.
	redacted := r.redactCiting("doc-1")
	if len(redacted) < 2 || redacted[0] != quoting.ID || redacted[len(redacted)-1] != given.ID {
		t.Fatalf("redacted %v, want the quoting reply %s through the reply given the summary %s", redacted, quoting.ID, given.ID)
	}
	deleted := r.eraseLedger("doc-1")
	var sumsDeleted int
	for _, tool := range deleted {
		if tool == toolSummary {
			sumsDeleted++
		}
	}
	if sumsDeleted < covering {
		t.Fatalf("the erase deleted %d summaries, want every one of the %d that condensed the value", sumsDeleted, covering)
	}
	folded = nil
	after, h := r.turn(t, "remind me")
	if s := h.Payload.Metadata[turnKeySummary]; strings.Contains(s, "HERON-7") {
		t.Fatalf("the turn after the forget was given the forgotten value: %q", s)
	}
	if tr := h.Payload.Metadata["transcript"]; strings.Contains(tr, "HERON-7") {
		t.Fatalf("the transcript after the forget holds the value: %q", tr)
	}
	// Whatever is folded again (the redacted replies are short, so the transcript may
	// now fit without a fold) never holds the value.
	for _, f := range folded {
		if strings.Contains(f, "HERON-7") {
			t.Fatalf("the recomputed summary condensed the forgotten value: %q", f)
		}
	}
	if len(after.SourceRefs) != 0 {
		t.Errorf("the reply after the forget rests on %v", after.SourceRefs)
	}
}

func TestTheTurnIndexEntryRecordsTheTurnsSources(t *testing.T) {
	r := newGroundingRig(t, Grounding{TurnIndex: true})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		turn := r.lastUserID()
		r.recall(turn, "doc-1")
		r.results[turn] = []domain.ExecuteResult{o3xResult()}
	}
	r.turn(t, "Which customers are more than 60 days overdue?")
	idx := r.ledger.tool(toolTurnIndex)
	if len(idx) != 1 || !reflect.DeepEqual(idx[0].SourceRefs, []string{"doc-1"}) {
		t.Fatalf("turn index entries = %+v, want one resting on doc-1", idx)
	}
	// The next turn is shown the index: its reply rests on it.
	r.pool.onDispatch = nil
	next, _ := r.turn(t, "thanks")
	if !reflect.DeepEqual(next.SourceRefs, []string{"doc-1"}) {
		t.Fatalf("a reply shown the index rests on %v", next.SourceRefs)
	}
}

func TestTheReplyCheckRecordRestsOnTheReplysSources(t *testing.T) {
	r := newGroundingRig(t, Grounding{ReplyCheck: ReplyCheckObserve})
	r.pool.onDispatch = func(h *domain.Handoff, call int) { r.recall(r.lastUserID(), "doc-1") }
	r.pool.replies = []string{"The total is 66483.75."}
	r.turn(t, "total?")
	checks := r.ledger.tool(toolReplyCheck)
	if len(checks) == 0 || !reflect.DeepEqual(checks[0].SourceRefs, []string{"doc-1"}) {
		t.Fatalf("reply check records = %+v, want resting on doc-1", checks)
	}
}

// A message Emit posts while a turn runs (the consent bridge's machine prompt, built
// from a step of that turn) rests on what the turn recalled and was given so far.
func TestEmitDuringATurnRecordsThatTurnsSources(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	var emitted domain.Message
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		switch call {
		case 1:
			r.recall(r.lastUserID(), "doc-0")
		case 2:
			r.recall(r.lastUserID(), "doc-1")
			var err error
			emitted, err = r.svc.Emit(context.Background(), "c1", "Cambrian wants to write C:/HERON-7.txt. Reply 'approve' or 'deny'.")
			if err != nil {
				panic(fmt.Sprintf("emit: %v", err))
			}
		}
	}
	r.turn(t, "q0")
	r.turn(t, "q1")
	if !reflect.DeepEqual(emitted.SourceRefs, []string{"doc-0", "doc-1"}) {
		t.Fatalf("emitted during the turn rests on %v, want the turn's recall and its transcript's", emitted.SourceRefs)
	}
	// After the turn, nothing runs: an emit rests on nothing.
	quiet, err := r.svc.Emit(context.Background(), "c1", "heads up")
	if err != nil || len(quiet.SourceRefs) != 0 {
		t.Fatalf("emit with no turn running = %+v, %v", quiet.SourceRefs, err)
	}
}
