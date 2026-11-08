package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// humanFailure maps a turn error onto one short sentence a user can act on, in the
// conversation's language (lang; locale.go — a catalogue, never a model translation).
//
// The mapping exists for the same reason the ADR-0098 progress vocabulary does: internal
// error text names leases, pools, RPC codes and agent ids, none of which mean anything to
// the person waiting and some of which disclose how the deployment is put together. A
// closed set of phrasings is the boundary.
//
// Silence is the one option that is never acceptable. A turn that fails without saying so
// is indistinguishable from a turn that hung, and the user is left guessing whether to
// wait or retry — which is the worst of both.
// ErrTurnStalled means the turn stopped reporting any activity and was cut loose.
//
// Deliberately distinct from a timeout: a timeout means "this took too long", a stall
// means "this stopped doing anything". The user can act on the difference — a stalled
// request is usually worth retrying immediately, a timed-out one usually needs narrowing.
var ErrTurnStalled = errors.New("chat: turn stopped making progress")

func humanFailure(err error, lang string) string {
	if err == nil {
		return ""
	}
	return Message(failureMessage(err), lang)
}

// failureMessage picks the catalogue sentence for a turn error (locale.go holds the text in
// every supported language).
func failureMessage(err error) MessageID {
	switch {
	case errors.Is(err, ErrTurnStalled):
		return MsgTurnStalled
	case errors.Is(err, ErrEmptyReply):
		return MsgEmptyReply
	case errors.Is(err, context.DeadlineExceeded):
		return MsgTurnTimeout
	case errors.Is(err, context.Canceled):
		return MsgTurnCancelled
	}

	// Fall back to shape rather than exact identity, so errors from layers this package
	// does not import still land somewhere sensible.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "busy"), strings.Contains(msg, "capacity"):
		return MsgAtCapacity
	case strings.Contains(msg, "session not found"), strings.Contains(msg, "lease"):
		return MsgSessionLost
	case strings.Contains(msg, "deadline"), strings.Contains(msg, "timeout"):
		return MsgTookTooLong
	}
	return MsgGenericFail
}

// finalAnswerEnvelope is the structured output an SDK ReAct loop produces on its last
// round. It normally never reaches here — the agent extracts the answer itself — but a
// fallback path can hand the envelope through verbatim, and a user should never be shown
// raw JSON.
type finalAnswerEnvelope struct {
	Action string `json:"action"`
	Answer string `json:"answer"`
}

// looksLikeControlEnvelope reports whether a reply is a ReAct control instruction that
// escaped the agent loop — `{"action": "memory_query", ...}` and friends.
//
// Reaching a human, these are pure noise: the user sees machine JSON and the step the model
// asked for never happened. The agent loop is where that is properly fixed, but a
// presentation layer that can show raw control JSON to a customer is a defect in its own
// right, so this is a last line of defence rather than the cure.
func looksLikeControlEnvelope(reply string) bool {
	trimmed := strings.TrimSpace(reply)
	if !strings.Contains(trimmed, `"action"`) {
		return false
	}
	// Scan rather than requiring a leading brace: the envelope usually TRAILS a sentence
	// of the model narrating itself. Same shape unwrapFinalAnswer handles, and the reason
	// a first version of this check missed the very case it was written for.
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] != '{' {
			continue
		}
		var env finalAnswerEnvelope
		if err := json.Unmarshal([]byte(trimmed[i:]), &env); err != nil {
			continue
		}
		// A final_answer is legitimate — unwrapFinalAnswer handles it. Any OTHER action is
		// a loop instruction that should never have been rendered.
		return env.Action != "" && env.Action != "final_answer"
	}
	return false
}

// unwrapFinalAnswer returns the human-readable answer inside a ReAct envelope, or the
// input unchanged.
//
// Two shapes are seen in practice. The envelope may be the whole payload, or — more often —
// it trails a sentence or two of the model narrating itself:
//
//	The file has been created successfully. Let me give you the summary.
//
//	{"action": "final_answer", "answer": "...", "type": "text"}
//
// Both reach the user as raw JSON if nothing unwraps them, and the preamble is thinking-out-
// loud that the answer field already supersedes — so the answer alone is what to keep.
//
// Deliberately narrow: it unwraps ONLY an object whose action is final_answer and whose
// answer is a non-empty string. A broader "if it looks like JSON, dig around in it" rule
// would eventually mangle a legitimate reply that happens to be JSON — a real case, since
// users do ask for JSON.
func unwrapFinalAnswer(reply string) string {
	trimmed := strings.TrimSpace(reply)
	if !strings.Contains(trimmed, "final_answer") {
		return reply // cheap reject: the overwhelmingly common case
	}

	// Try the whole payload first, then progressively later `{` positions. Scanning from
	// the LAST candidate backwards finds the trailing envelope without being confused by
	// braces inside the preamble.
	starts := []int{}
	for i, r := range trimmed {
		if r == '{' {
			starts = append(starts, i)
		}
	}
	for i := len(starts) - 1; i >= 0; i-- {
		var env finalAnswerEnvelope
		if err := json.Unmarshal([]byte(trimmed[starts[i]:]), &env); err != nil {
			continue
		}
		if env.Action != "final_answer" {
			continue
		}
		if answer := strings.TrimSpace(env.Answer); answer != "" {
			return answer
		}
	}
	return reply
}
