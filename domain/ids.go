package domain

// Identity types.
//
// These are distinct named types, not aliases, so the compiler rejects the substitution that
// caused the Phase-0 defect class: a per-step BudgetLease being passed where a durable task
// Session was expected. The two are both opaque strings and were freely interchangeable, so
// the mistake was invisible at every call site and at review.
//
// Conversion to and from plain strings is explicit and deliberately unglamorous — at the
// storage, proto and metadata edges you write string(id) / SessionID(s), and that visible
// cast is the reminder that you are leaving the typed world.
//
// WHERE THESE TYPES APPLY (the line is deliberate, not partial work):
//
//   - TYPED wherever an ID is used to LOOK SOMETHING UP or GATE ACCESS — the lease
//     registry, session lookup, caller-scope re-derivation, content-node ownership,
//     checkpoint keys, the execution control hub. These are the places a wrong ID causes a
//     wrong answer, so the compiler should be the one to catch it.
//
//   - PLAIN STRING at pure reporting and serialization edges — telemetry events, operator
//     feed payloads, proto fields, and document metadata. Those values are write-only sinks
//     that become strings anyway; typing them adds churn and prevents no bug.
//
// One hard rule at the metadata edge: Document.Metadata is map[string]interface{}, read back
// with `.(string)` assertions. Storing a TYPED id there satisfies none of them and silently
// breaks every reader — so always write string(id). TestSessionID_MustBeStringInDocumentMetadata
// pins this; it exists because typing an internal field caused exactly that failure.
type (
	// SessionID identifies a durable, goal-scoped task Session. Lifetime: until the
	// session completes — many plan runs.
	SessionID string

	// RunID identifies ONE plan execution (one Execute call) within a session.
	// Lifetime: one call.
	RunID string

	// LeaseID identifies one per-step BudgetLease (ADR-0018). Lifetime: one step.
	// It is a CREDENTIAL, not a designation: an agent presents it, and the kernel
	// resolves it to the session/run/step it was bound to.
	LeaseID string
)

// String returns the underlying value. Present so IDs format cleanly in logs and errors
// without an explicit conversion at every call site.
func (id SessionID) String() string { return string(id) }
func (id RunID) String() string     { return string(id) }
func (id LeaseID) String() string   { return string(id) }
