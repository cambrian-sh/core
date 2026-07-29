package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// capturingSink records every progress snapshot a turn emits.
type capturingSink struct{ got []domain.ProgressUpdate }

func (c *capturingSink) Progress(_ context.Context, u domain.ProgressUpdate) {
	c.got = append(c.got, u)
}

func (c *capturingSink) phases() []domain.ProgressPhase {
	out := make([]domain.ProgressPhase, 0, len(c.got))
	for _, u := range c.got {
		out = append(out, u.Phase)
	}
	return out
}

// THE invariant of ADR-0098 (D1). Progress is delivered to the human and never enters the
// record the model reads back. If this test ever fails, every subsequent turn is being fed
// the system's narration of itself, and conversation quality degrades for a reason that is
// invisible from the outside.
func TestProgress_NeverEntersTheTranscript(t *testing.T) {
	svc, store, _ := setup(t, domain.ProfileEmployee)
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if len(sink.got) == 0 {
		t.Fatal("expected progress to be emitted; the sink saw nothing")
	}

	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 {
		t.Fatalf("expected exactly user + agent in the transcript, got %d: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Role != domain.MessageRoleUser && m.Role != domain.MessageRoleAgent {
			t.Errorf("unexpected role in transcript: %q", m.Role)
		}
		// The rendered progress text must appear nowhere in stored content. The terminal
		// update renders empty by design, and every string contains the empty string —
		// so checking it would assert nothing while appearing to assert everything.
		for _, u := range sink.got {
			text := u.Text()
			if text == "" {
				continue
			}
			if strings.Contains(m.Content, text) {
				t.Errorf("progress text %q leaked into a stored message: %q", text, m.Content)
			}
		}
	}
}

// A second turn must not see the first turn's progress in its history window. This is the
// consequence the invariant exists to prevent, asserted end to end.
func TestProgress_AbsentFromNextTurnsHistory(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	ctx := context.Background()
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "first"}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "second"}); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	// The transcript handed to the agent is in the handoff metadata. Check EVERY dispatch,
	// not just the last: a leak on any turn is a leak.
	if pool.calls() < 2 {
		t.Fatalf("expected 2 dispatches, got %d", pool.calls())
	}
	transcript := ""
	for _, h := range pool.all() {
		for _, v := range h.Context {
			transcript += v
		}
		if h.Payload != nil {
			transcript += string(h.Payload.Data)
		}
	}
	for _, phase := range []domain.ProgressPhase{
		domain.PhaseUnderstanding, domain.PhaseWorking, domain.PhaseWriting,
	} {
		if strings.Contains(transcript, string(phase)) {
			t.Errorf("phase %q reached the agent's transcript — context is being polluted", phase)
		}
	}
}

// The user must hear something before the slow part starts, and the answer must supersede
// it (ADR-0098 D3) rather than leaving "working on it" as the last thing on screen.
func TestProgress_PhaseOrderBracketsTheSlowPart(t *testing.T) {
	svc, _, _ := setup(t, domain.ProfileEmployee)
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	// Separate the phase-bearing snapshots from the terminal clear that always follows.
	var phases []domain.ProgressPhase
	for _, u := range sink.got {
		if u.Final {
			continue
		}
		phases = append(phases, u.Phase)
	}

	if len(phases) < 3 {
		t.Fatalf("expected at least understanding/working/writing, got %v", phases)
	}
	if phases[0] != domain.PhaseUnderstanding {
		t.Errorf("first phase should acknowledge immediately, got %q", phases[0])
	}
	if last := phases[len(phases)-1]; last != domain.PhaseWriting {
		t.Errorf("last phase before the reply should be %q, got %q", domain.PhaseWriting, last)
	}
	// Every phase-bearing emission is inside the closed vocabulary — no internal state
	// escaped. Terminal updates carry no phase by design and are excluded above.
	for _, p := range phases {
		if !p.Valid() {
			t.Errorf("emitted phase %q is outside the closed vocabulary", p)
		}
	}
	// And the very last thing on the wire is always the clear.
	if !sink.got[len(sink.got)-1].Final {
		t.Error("the final emission must be the terminal clear")
	}
}

// ADR-0098 D5: the observer must never break the observed. A turn with no sink wired — the
// OSS default — must behave exactly as before.
func TestProgress_WithoutASinkTheTurnIsUnchanged(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	// deliberately no SetProgressSink

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if err != nil {
		t.Fatalf("Turn without a progress sink: %v", err)
	}
	if got.Content != "hello there" {
		t.Errorf("unexpected reply: %q", got.Content)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}
	if pool.calls() != 1 {
		t.Errorf("expected 1 dispatch, got %d", pool.calls())
	}
}

// The bug that stranded a user on "working on it": a turn that fails before replying used
// to leave the status line up forever, which reads as a hang. Every exit path must emit
// the terminal clear.
func TestProgress_TerminalClearOnFailurePaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"empty reply", ""},          // ErrEmptyReply — returns before the success path
		{"normal reply", "hello there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, pool := setup(t, domain.ProfileEmployee)
			pool.reply = tc.reply
			sink := &capturingSink{}
			svc.SetProgressSink(sink)

			_, _ = svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})

			if len(sink.got) == 0 {
				t.Fatal("expected progress to be emitted")
			}
			last := sink.got[len(sink.got)-1]
			if !last.Final {
				t.Errorf("last emission must be terminal, got phase %q", last.Phase)
			}
			// Success clears the line; failure leaves an explanation on it.
			if tc.reply == "" {
				if last.Text() == "" {
					t.Error("a failed turn must end the line with an explanation, not silence")
				}
			} else if last.Text() != "" {
				t.Errorf("a successful turn must clear the line, got %q", last.Text())
			}
		})
	}
}

// A dispatch failure is the other way a turn ends without a reply.
func TestProgress_TerminalClearWhenDispatchFails(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	pool.err = errors.New("pool busy")
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err == nil {
		t.Fatal("expected the dispatch error to propagate")
	}
	if len(sink.got) == 0 || !sink.got[len(sink.got)-1].Final {
		t.Error("a failed dispatch must still take the status line down")
	}
}
