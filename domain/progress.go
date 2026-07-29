package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ProgressPhase is a human-facing description of what the system is doing right now.
//
// It is a CLOSED vocabulary (ADR-0098 D7). Internal state — agent ids, capability strings,
// tool names, plan internals, model names — is mapped onto one of these at the emission
// seam and never crosses it raw. Two reasons, and the second is the serious one: internal
// names mean nothing to an end user, and on a customer-facing surface they disclose the
// shape of the deployment. The mapping is where that boundary lives.
type ProgressPhase string

const (
	// PhaseUnderstanding covers routing and intent classification — everything before
	// there is a plan to report steps against.
	PhaseUnderstanding ProgressPhase = "understanding the request"
	// PhasePlanning covers plan construction.
	PhasePlanning ProgressPhase = "planning the work"
	// PhaseSearching covers memory retrieval in any of its tiers.
	PhaseSearching ProgressPhase = "searching memory"
	// PhaseRunningTool covers tool execution, whatever the tool is.
	PhaseRunningTool ProgressPhase = "running a tool"
	// PhaseWorking is the generic step phase — the fallback when a plan step maps to
	// nothing more specific. A new internal phase with no mapping degrades to this,
	// never to a raw internal string (ADR-0098 D7).
	PhaseWorking ProgressPhase = "working on it"
	// PhaseWriting covers final answer synthesis.
	PhaseWriting ProgressPhase = "writing the answer"
)

// Valid reports whether p is a member of the closed vocabulary. An update carrying an
// unknown phase is a programming error at the emission seam, not something to render.
func (p ProgressPhase) Valid() bool {
	switch p {
	case PhaseUnderstanding, PhasePlanning, PhaseSearching,
		PhaseRunningTool, PhaseWorking, PhaseWriting:
		return true
	}
	return false
}

// ProgressUpdate is one supersedable snapshot of what a conversation's turn is doing.
//
// It is a SNAPSHOT, not an event: each update replaces the last rather than appending to a
// log (ADR-0098 D2). That is what keeps a twenty-six-round turn to a single evolving line
// instead of twenty-six messages, and it is why nothing here carries a sequence number.
type ProgressUpdate struct {
	ConversationID string
	// Step is 1-based for display ("step 2 of 4"). Zero when there is no plan yet.
	Step int
	// TotalSteps is 0 while the plan is still unknown — a caller renders "working on it"
	// rather than inventing a denominator.
	TotalSteps int
	Phase      ProgressPhase
	UpdatedAt  time.Time
	// Final marks the turn as over — successfully or not — and means "take the status
	// line down" (ADR-0098 D3).
	//
	// It exists because the happy path is not the only path. A reply supersedes progress
	// naturally, but a turn that fails before replying delivers nothing, and without a
	// terminal signal the user is left staring at "working on it" forever. That is a
	// worse failure than showing no progress at all, because it looks like a hang.
	//
	// A final update renders as EMPTY text, and empty progress text means clear.
	Final bool
	// Note is the closing line a FINAL update leaves on screen instead of clearing —
	// used to say what went wrong.
	//
	// It rides the progress channel rather than becoming a message on purpose. A failure
	// notice belongs in front of the user, but not in the transcript: persisting it would
	// feed "something went wrong" back into the model's context on the next turn, and it
	// would break the existing invariant that a failed turn stores nothing.
	Note string
}

// Text renders the update as one short human-facing line.
//
// Deliberately plain: this string reaches end users on surfaces we do not control the
// formatting of, so it carries no markup, no internal identifiers, and no punctuation that
// a transport might interpret.
func (u ProgressUpdate) Text() string {
	if u.Final {
		// A note leaves the line up saying why; no note clears it.
		return strings.TrimSpace(u.Note)
	}
	phase := u.Phase
	if !phase.Valid() {
		phase = PhaseWorking
	}
	if u.TotalSteps > 0 && u.Step > 0 {
		return string(phase) + " — step " + itoa(u.Step) + " of " + itoa(u.TotalSteps)
	}
	return string(phase)
}

// Validate checks the invariants a sink may rely on.
func (u ProgressUpdate) Validate() error {
	if strings.TrimSpace(u.ConversationID) == "" {
		return errProgressNoConversation
	}
	if !u.Final && !u.Phase.Valid() {
		return errProgressBadPhase
	}
	return nil
}

// Validation errors. Kept as sentinels so a sink can distinguish a malformed update
// from a transport failure without string matching.
var (
	errProgressNoConversation = errors.New("progress: ConversationID is required")
	errProgressBadPhase       = errors.New("progress: Phase is outside the closed vocabulary")
)

// ProgressSink receives progress snapshots for a conversation.
//
// The kernel EMITS unconditionally through this port; whether anyone is listening is not
// its concern (ADR-0098 D8). The OSS default is a no-op, and a premium bridge substitutes a
// real implementation — the same shape as the ADR-0085 authorizer split, where the kernel
// always asks and the plugin supplies the answer.
//
// Implementations MUST NOT block and MUST NOT return an error. Progress is best-effort by
// construction (ADR-0098 D5): a telemetry channel that can stall or fail the work it
// describes is worse than no telemetry channel. Anything that could fail — delivery,
// rate limiting, transport errors — belongs behind this call, not in front of it.
type ProgressSink interface {
	Progress(ctx context.Context, u ProgressUpdate)
}

// NoopProgressSink discards every update. It is the OSS default so that the emission seam
// is always safe to call without a nil check at each site.
type NoopProgressSink struct{}

// Progress discards the update.
func (NoopProgressSink) Progress(context.Context, ProgressUpdate) {}

// EmitProgress is the call sites' helper: it tolerates a nil sink and drops invalid
// updates rather than propagating a programming error into a user-visible surface.
//
// Call sites are on the hot path of plan execution, so this deliberately does nothing
// expensive and never returns anything to check.
func EmitProgress(ctx context.Context, sink ProgressSink, u ProgressUpdate) {
	if sink == nil {
		return
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = time.Now()
	}
	if err := u.Validate(); err != nil {
		return
	}
	sink.Progress(ctx, u)
}

// itoa is a tiny local integer formatter — avoids pulling strconv into the domain package
// for two call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
