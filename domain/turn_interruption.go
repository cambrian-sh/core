package domain

// Person-facing turn failures (ENG-2124, owner decision 2026-11-18): a chat turn that
// fails or is interrupted shows the person one plain, short sentence and leaves an
// interruption marker in the conversation; the raw cause — provider JSON, endpoint URLs,
// socket paths, RPC status text — goes to the logs only.

// ProviderFault is the class of the last model-provider failure seen on a lease, as the
// LLM gateway classified it (the same classes its gRPC status carries since ENG-1005).
// The kernel records it on the lease, so the chat lane can tell the person what happened
// even when the worker that made the call reports every failure as one opaque error.
type ProviderFault string

const (
	// ProviderFaultNone: no provider failure recorded (or the last call succeeded).
	ProviderFaultNone ProviderFault = ""
	// ProviderFaultAuth: the provider refused the deployment's credential (ErrProviderAuth).
	ProviderFaultAuth ProviderFault = "provider_auth"
	// ProviderFaultRateLimited: the provider rate-limited us past the transport backoff
	// (HTTP 429), or the gateway itself was saturated.
	ProviderFaultRateLimited ProviderFault = "provider_rate_limited"
	// ProviderFaultUnavailable: the provider answered 5xx, never answered, or no model
	// is healthy.
	ProviderFaultUnavailable ProviderFault = "provider_unavailable"
	// ProviderFaultSpendLimit: the session's spend ceiling refused the call (ADR-0131).
	ProviderFaultSpendLimit ProviderFault = "provider_spend_limit"
	// ProviderFaultContextOverflow: the request was larger than the model's context.
	ProviderFaultContextOverflow ProviderFault = "context_overflow"
	// ProviderFaultNotConfigured: no model is allocated to the step and no default model
	// is configured — a deployment setting, not an outage (ENG-2156). Distinct from
	// ProviderFaultUnavailable because waiting does not help; an operator must fix it.
	ProviderFaultNotConfigured ProviderFault = "provider_not_configured"
)

// TurnInterruptReason names why a chat turn ended without a reply. It is the key of the
// chat lane's message table (internal/chat/locale.go) and of the operator plane's status
// code; adding one is a table entry in both, never a branch on message text.
type TurnInterruptReason string

const (
	TurnInterruptProviderAuth        TurnInterruptReason = "provider_auth"
	TurnInterruptProviderBusy        TurnInterruptReason = "provider_busy"
	TurnInterruptProviderUnavailable TurnInterruptReason = "provider_unavailable"
	TurnInterruptNotConfigured       TurnInterruptReason = "not_configured"
	TurnInterruptSpendLimit          TurnInterruptReason = "spend_limit"
	TurnInterruptContextTooLarge     TurnInterruptReason = "context_too_large"
	TurnInterruptAssistantRestarted  TurnInterruptReason = "assistant_restarted"
	// ENG-1634 (owner decision 2026-11-20): a turn whose worker was killed is
	// re-driven once, only when the killed attempt began no side-effecting call.
	// These two end it instead: the attempt had already begun an action (a re-run
	// could do it twice), or the re-drive's worker was killed too.
	TurnInterruptAssistantRestartedAfterAction TurnInterruptReason = "assistant_restarted_after_action"
	TurnInterruptAssistantRestartedTwice       TurnInterruptReason = "assistant_restarted_twice"
	TurnInterruptStalled                       TurnInterruptReason = "stalled"
	TurnInterruptTimeout                       TurnInterruptReason = "timeout"
	TurnInterruptCancelled                     TurnInterruptReason = "cancelled"
	TurnInterruptAtCapacity                    TurnInterruptReason = "at_capacity"
	TurnInterruptSessionLost                   TurnInterruptReason = "session_lost"
	TurnInterruptEmptyReply                    TurnInterruptReason = "empty_reply"
	TurnInterruptFailed                        TurnInterruptReason = "failed"
)

// TurnInterruption is the error a chat turn returns when it ends without a reply.
//
// Message is the only text a person may be shown. Error() carries the cause too, for
// logs and wrapping; a transport that answers a person (the operator plane's
// mapConvError) must use Message, never Error().
type TurnInterruption struct {
	Reason  TurnInterruptReason
	Message string
	Cause   error
}

func (e *TurnInterruption) Error() string {
	if e.Cause != nil {
		return "chat turn interrupted (" + string(e.Reason) + "): " + e.Cause.Error()
	}
	return "chat turn interrupted (" + string(e.Reason) + ")"
}

func (e *TurnInterruption) Unwrap() error { return e.Cause }
