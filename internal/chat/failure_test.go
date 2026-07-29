package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestHumanFailure_NeverLeaksInternals(t *testing.T) {
	// Real error text from the running system. None of it belongs in front of a user.
	internal := []error{
		errors.New("session not found: lease-1785277563127085900-445"),
		errors.New("rpc error: code = Internal desc = CallDaemon: no daemon registered for stream \"telegram_ingress_agent\""),
		errors.New("agentpool: ErrPoolBusy"),
		errors.New("context deadline exceeded"),
		ErrEmptyReply,
	}
	leaky := []string{"lease-", "rpc error", "code =", "CallDaemon", "agentpool", "_agent", "stream"}

	for _, err := range internal {
		got := humanFailure(err)
		if got == "" {
			t.Errorf("every failure must say something; %v produced silence", err)
			continue
		}
		for _, bad := range leaky {
			if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
				t.Errorf("failure notice %q leaked %q from %v", got, bad, err)
			}
		}
	}
}

func TestHumanFailure_NilIsSilent(t *testing.T) {
	if got := humanFailure(nil); got != "" {
		t.Errorf("a successful turn must produce no notice, got %q", got)
	}
}

func TestHumanFailure_RecognisesTheCommonCases(t *testing.T) {
	cases := []struct {
		err  error
		want string // a distinguishing fragment
	}{
		{ErrEmptyReply, "rephrasing"},
		{context.DeadlineExceeded, "longer than"},
		{errors.New("session not found: lease-123"), "working session"},
		{errors.New("pool busy"), "capacity"},
	}
	for _, tc := range cases {
		if got := humanFailure(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("humanFailure(%v) = %q, expected it to mention %q", tc.err, got, tc.want)
		}
	}
}

func TestUnwrapFinalAnswer(t *testing.T) {
	// The exact envelope a user was shown verbatim.
	leaked := `{"action": "final_answer", "answer": "Current date and time: 2026-07-28 22:57:58 UTC", "type": "text"}`
	if got := unwrapFinalAnswer(leaked); got != "Current date and time: 2026-07-28 22:57:58 UTC" {
		t.Errorf("expected the answer field, got %q", got)
	}

	// The shape actually seen in production: the model narrates, then emits the envelope.
	trailing := "The file has been created successfully. Let me give you the summary.\n\n" +
		`{"action": "final_answer", "answer": "telegram.md has been created.", "type": "text"}`
	if got := unwrapFinalAnswer(trailing); got != "telegram.md has been created." {
		t.Errorf("expected the trailing envelope to be unwrapped, got %q", got)
	}

	// A legitimate JSON reply must survive untouched — users do ask for JSON, and a
	// broader "dig around inside any object" rule would eventually mangle one.
	for _, passthrough := range []string{
		`{"name": "afsin", "role": "founder"}`,
		`{"action": "something_else", "answer": "no"}`,
		`{"action": "final_answer", "answer": ""}`,
		"plain text answer",
		"",
		`{not valid json`,
	} {
		if got := unwrapFinalAnswer(passthrough); got != passthrough {
			t.Errorf("unwrapFinalAnswer(%q) altered it to %q", passthrough, got)
		}
	}
}

// End to end: a failing turn must leave the user with an explanation, not silence — and it
// must do so WITHOUT persisting anything, so the transcript invariant survives and the
// model is not fed "something went wrong" on the next turn.
func TestTurn_FailureIsReportedToTheUser(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	pool.reply = "" // triggers ErrEmptyReply
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if err == nil {
		t.Fatal("expected the turn to fail")
	}

	// The explanation reaches the user on the progress line.
	if len(sink.got) == 0 {
		t.Fatal("expected progress emissions")
	}
	last := sink.got[len(sink.got)-1]
	if !last.Final {
		t.Fatal("the last emission must be terminal")
	}
	if last.Text() == "" {
		t.Error("a failed turn must end the line with an explanation, not silence")
	}
	if strings.Contains(last.Text(), "ErrEmptyReply") {
		t.Errorf("notice leaked the internal error name: %q", last.Text())
	}

	// And nothing was persisted beyond the user's own message.
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 1 {
		t.Errorf("a failed turn must store nothing but the user message, got %d: %+v", len(msgs), msgs)
	}
}

// A successful turn must NOT produce a failure notice.
func TestTurn_SuccessProducesNoNotice(t *testing.T) {
	svc, store, _ := setup(t, domain.ProfileEmployee)

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 {
		t.Fatalf("expected exactly user + reply, got %d: %+v", len(msgs), msgs)
	}
}

// The leaked envelope must be unwrapped on the real path, not just in the helper.
func TestTurn_UnwrapsALeakedReActEnvelope(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	pool.reply = `{"action": "final_answer", "answer": "it is 3pm", "type": "text"}`

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "time?"})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got.Content != "it is 3pm" {
		t.Errorf("expected the unwrapped answer, got %q", got.Content)
	}
}

// A ReAct control envelope must never reach a user. This is the exact string one did.
func TestLooksLikeControlEnvelope(t *testing.T) {
	leaked := []string{
		`{"action": "memory_query", "query": "airline"}`,
		// The shape that got through a first version of this check: prose, THEN the
		// envelope. Requiring a leading brace missed the very case it was written for.
		"I'll query my memory for airline-related content in detail.\n\n" +
			`{"action": "memory_query", "query": "airline aviation airport flight"}`,
		"Let me look that up.\n" + `{"action": "tool_call", "tool": "web_search", "args": {}}`,
		`{"action": "tool_call", "tool": "web_search", "args": {"query": "x"}}`,
		`{"action": "find_tools", "need": "search the web"}`,
		`{"action": "yield_subgoal", "intent": "summarise"}`,
	}
	for _, s := range leaked {
		if !looksLikeControlEnvelope(s) {
			t.Errorf("expected %q to be recognised as a control envelope", s)
		}
	}

	// A real answer must pass through — including a legitimate JSON answer, since users
	// do ask for JSON, and including final_answer which unwrapFinalAnswer handles.
	for _, s := range []string{
		`{"action": "final_answer", "answer": "hello", "type": "text"}`,
		`{"name": "afsin", "role": "founder"}`,
		`{"status": "ok"}`,
		"plain prose",
		"",
	} {
		if looksLikeControlEnvelope(s) {
			t.Errorf("%q is not a control envelope but was flagged as one", s)
		}
	}
}

// End to end: a leaked envelope becomes a readable failure, never machine JSON.
func TestTurn_ControlEnvelopeBecomesAFailureNotJSON(t *testing.T) {
	svc, store, pool := setup(t, domain.ProfileEmployee)
	pool.reply = `{"action": "memory_query", "query": "airline"}`
	sink := &capturingSink{}
	svc.SetProgressSink(sink)

	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "check memory"})
	if err == nil {
		t.Fatal("a control envelope is not an answer; the turn should fail")
	}

	// Nothing machine-shaped was stored or shown.
	msgs, _ := store.ListMessages(context.Background(), "c1", 0, 0)
	for _, m := range msgs {
		if strings.Contains(m.Content, `"action"`) {
			t.Errorf("control JSON reached the transcript: %q", m.Content)
		}
	}
	last := sink.got[len(sink.got)-1]
	if !last.Final || last.Text() == "" {
		t.Error("the user should get a readable explanation, not silence")
	}
	if strings.Contains(last.Text(), `"action"`) {
		t.Errorf("control JSON reached the user: %q", last.Text())
	}
}
