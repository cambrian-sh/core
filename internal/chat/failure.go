package chat

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
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
	var ti *domain.TurnInterruption
	if errors.As(err, &ti) && ti.Message != "" {
		return ti.Message // already said in the turn's language
	}
	return Message(failureMessage(err), lang)
}

// interruptMessages is the one table from a failure class to the sentence the person is
// shown (ENG-2124; the text itself is in locale.go, per language). A class missing here
// says the generic sentence; TestInterruptMessages_EveryReasonHasASentence holds the
// table complete.
var interruptMessages = map[domain.TurnInterruptReason]MessageID{
	domain.TurnInterruptProviderAuth:                  MsgProviderAuth,
	domain.TurnInterruptProviderBusy:                  MsgProviderBusy,
	domain.TurnInterruptProviderUnavailable:           MsgProviderUnavailable,
	domain.TurnInterruptNotConfigured:                 MsgNotConfigured,
	domain.TurnInterruptSpendLimit:                    MsgSpendLimit,
	domain.TurnInterruptContextTooLarge:               MsgContextTooLarge,
	domain.TurnInterruptAssistantRestarted:            MsgAssistantRestarted,
	domain.TurnInterruptAssistantRestartedAfterAction: MsgAssistantRestartedAfterAction,
	domain.TurnInterruptAssistantRestartedTwice:       MsgAssistantRestartedTwice,
	domain.TurnInterruptStalled:                       MsgTurnStalled,
	domain.TurnInterruptTimeout:                       MsgTurnTimeout,
	domain.TurnInterruptCancelled:                     MsgTurnCancelled,
	domain.TurnInterruptAtCapacity:                    MsgAtCapacity,
	domain.TurnInterruptSessionLost:                   MsgSessionLost,
	domain.TurnInterruptEmptyReply:                    MsgEmptyReply,
	domain.TurnInterruptFailed:                        MsgGenericFail,
}

// providerFaultReasons maps the provider-failure class the LLM gateway recorded on the
// turn's lease (domain.BudgetLeaseState.LastFault) to the turn's failure class.
var providerFaultReasons = map[domain.ProviderFault]domain.TurnInterruptReason{
	domain.ProviderFaultAuth:            domain.TurnInterruptProviderAuth,
	domain.ProviderFaultRateLimited:     domain.TurnInterruptProviderBusy,
	domain.ProviderFaultUnavailable:     domain.TurnInterruptProviderUnavailable,
	domain.ProviderFaultSpendLimit:      domain.TurnInterruptSpendLimit,
	domain.ProviderFaultContextOverflow: domain.TurnInterruptContextTooLarge,
	domain.ProviderFaultNotConfigured:   domain.TurnInterruptNotConfigured,
}

// failureMessage picks the catalogue sentence for a turn error (locale.go holds the text in
// every supported language).
func failureMessage(err error) MessageID {
	return interruptMessage(failureReason(err, domain.ProviderFaultNone))
}

func interruptMessage(r domain.TurnInterruptReason) MessageID {
	if id, ok := interruptMessages[r]; ok {
		return id
	}
	return MsgGenericFail
}

// failureReason classifies a turn error. fault is the provider failure the gateway
// recorded on the turn's lease, if any: the kernel's own observation, so it holds even
// when the worker reported the failure as an opaque Internal error (until ENG-2125 the
// SDK worker reports every gateway failure that way).
//
// Precedence: the kernel's own verdicts on the turn (stalled, empty, deadline) first;
// then the lease's provider fault; then what the pool or the worker reported.
func failureReason(err error, fault domain.ProviderFault) domain.TurnInterruptReason {
	var ti *domain.TurnInterruption
	switch {
	case errors.As(err, &ti):
		return ti.Reason
	case errors.Is(err, ErrTurnStalled):
		return domain.TurnInterruptStalled
	case errors.Is(err, ErrEmptyReply):
		return domain.TurnInterruptEmptyReply
	case errors.Is(err, context.DeadlineExceeded):
		return domain.TurnInterruptTimeout
	}
	if r, ok := providerFaultReasons[fault]; ok {
		return r
	}
	switch {
	case errors.Is(err, context.Canceled):
		return domain.TurnInterruptCancelled
	case errors.Is(err, agentpool.ErrPoolBusy):
		return domain.TurnInterruptAtCapacity
	case errors.Is(err, agentpool.ErrWorkerLost):
		// The worker was gone when the turn reached it; the pool respawned it.
		return domain.TurnInterruptAssistantRestarted
	}
	// The worker's status. Unavailable is the transport breaking under the call: the
	// worker process died mid-turn (the readiness run's killed chat worker) and its
	// supervisor restarts it. A provider outage or a missing model the worker relays as
	// Unavailable (ENG-2125) has a lease fault recorded and was classified above
	// (ENG-2156).
	switch status.Code(err) {
	case codes.Unavailable:
		return domain.TurnInterruptAssistantRestarted
	case codes.Unauthenticated:
		return domain.TurnInterruptProviderAuth
	case codes.ResourceExhausted:
		return domain.TurnInterruptProviderBusy
	case codes.OutOfRange:
		// The gateway's answer to a prompt larger than the model's context
		// (domain.ErrContextOverflow), relayed by the worker (ENG-2156).
		return domain.TurnInterruptContextTooLarge
	case codes.PermissionDenied, codes.NotFound:
		// The gateway refused the turn's lease: foreign, expired or swept
		// (PermissionDenied "lease is not valid for this caller"), missing
		// (PermissionDenied "a lease is required", ENG-2171 — it was Unauthenticated
		// and read as a provider credential failure) or unknown (NotFound "session
		// not found") — ENG-2156.
		return domain.TurnInterruptSessionLost
	case codes.DeadlineExceeded:
		return domain.TurnInterruptTimeout
	case codes.Canceled:
		return domain.TurnInterruptCancelled
	}

	// Fall back to shape rather than exact identity, so errors from layers this package
	// does not import still land somewhere sensible.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "busy"), strings.Contains(msg, "capacity"), strings.Contains(msg, "saturated"):
		return domain.TurnInterruptAtCapacity
	case strings.Contains(msg, "session not found"), leaseWord.MatchString(msg):
		// A worker that reports the gateway's refusal as Internal (before ENG-2125).
		return domain.TurnInterruptSessionLost
	case strings.Contains(msg, "deadline"), strings.Contains(msg, "timeout"):
		return domain.TurnInterruptTimeout
	}
	return domain.TurnInterruptFailed
}

// leaseWord matches "lease" as a word: a bare substring test also matched "please" and
// "released", turning unrelated failures into a lost session (ENG-2156).
var leaseWord = regexp.MustCompile(`\blease\b`)

// interruption turns a failed turn's error into the person-facing *domain.TurnInterruption:
// the class, its plain sentence in lang, and the raw error as the cause (logs only).
func interruption(err error, fault domain.ProviderFault, lang string) *domain.TurnInterruption {
	var ti *domain.TurnInterruption
	if errors.As(err, &ti) {
		return ti
	}
	r := failureReason(err, fault)
	return &domain.TurnInterruption{Reason: r, Message: Message(interruptMessage(r), lang), Cause: err}
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
