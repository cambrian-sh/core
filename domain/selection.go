package domain

import (
	"context"
	"errors"
)

// ErrNoCandidates is returned by a ResourceSelector when there is nothing to
// bind an intent to.
var ErrNoCandidates = errors.New("selection: no candidates")

// ErrPinnedAgentUnavailable is returned when a HARD agent pin names an agent
// that is not registered or is not dispatchable (a daemon or a privileged system
// organ). A hard pin is an explicit user directive, so it fails loudly instead of
// quietly routing the step to some other agent — a soft pin is the mechanism for
// "prefer this one, but carry on without it".
var ErrPinnedAgentUnavailable = errors.New("selection: pinned agent unavailable")

// Intent is a capability-space description of a unit of work (ADR-0037 D3).
// Unlike DispatchTask it is agent-agnostic — the skeleton is drafted in intents
// and a concrete resource is bound to each at execution time. Embedding is the
// shared description-embedding vector used for capability retrieval (D2 index).
type Intent struct {
	ID          string
	Description string
	Embedding   []float32
	// RequiredCapabilities carries the ROUTE-03 capability contract into the
	// selector arms. Every selector rebuilds an DispatchTask from this Intent to
	// call the Gatekeeper, and a rebuild that omits this field silently disables
	// the L1 capability gate for that arm — measured 2026-07-28: with
	// resource_selector="efe" the contract reached the DispatchTask in the Server
	// and was then dropped by the rebuild, so L1 filtered nobody all run and an
	// agent declaring only `general_purpose` won file-handling steps. The gate is
	// a hard requirement, so it must survive every path that reaches L1.
	RequiredCapabilities []string

	// PreferredAgent / AgentPin carry the step's agent pin through the selector
	// arms, for the same reason RequiredCapabilities does: every selector rebuilds
	// an DispatchTask from this Intent, and whatever the rebuild omits is silently
	// dropped from selection.
	PreferredAgent string
	AgentPin       string
}

// PrecisionWeight is the precision-weighted belief about one resource's fitness
// for a given intent (ADR-0037 D2/D7). ExpectedSuccess is the pragmatic value;
// Confidence drives the epistemic (exploration) value — a low-confidence belief
// has high expected information gain. The Gatekeeper (D7) shapes these weights
// (policy non-compliance ⇒ ExpectedSuccess 0).
type PrecisionWeight struct {
	ResourceID      string
	ExpectedSuccess float64 // [0,1] — pragmatic value
	Confidence      float64 // [0,1] — 1-Confidence is the epistemic value
}

// PrecisionProvider resolves a candidate set into precision-weighted beliefs
// for an intent (ADR-0037 D2/D7). It is the seam between the belief store and
// the precision shaper. The EFE selector that once consumed it is gone; the
// provider survives because the belief store and shaper are independent of it.
type PrecisionProvider interface {
	PrecisionFor(ctx context.Context, intent Intent, candidates []AgentDefinition) ([]PrecisionWeight, error)
}
