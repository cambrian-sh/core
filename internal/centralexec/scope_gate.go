package centralexec

import (
	"errors"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ChildBuffer is the least-privilege episodic buffer a delegated child receives
// (ADR-0037 D11). It holds ONLY the explicitly-passed intent and payload — the
// parent's other step results and working memory are not fields here, so they
// cannot be inherited. "Scope-leak across delegation" is impossible because
// nothing is inherited; only a scope-checked payload crosses.
type ChildBuffer struct {
	Intent  string
	Payload *domain.Payload
}

// ErrPayloadUnreadable is returned when a sub-goal's payload lies outside the
// child's effective scope. The binding is then unsatisfiable for that resource
// — the CE re-selects or escalates; it never silently down-scopes or leaks.
var ErrPayloadUnreadable = errors.New("delegation: payload outside child's effective scope")

// InheritBuffer constructs a child's inbound buffer from a yielded sub-goal,
// gating the payload to the child's effective scope (ADR-0034). The payload's
// tags are kernel-derived (deterministic provenance, ADR-0034/0035), passed in —
// no LLM in the scope boundary. A nil scope is fail-closed. If the payload is
// not readable under the child's scope, the binding fails.
func InheritBuffer(sg SubGoal, payloadTags []string, childScope *domain.EffectiveScope) (ChildBuffer, error) {
	if !childScope.Allows(payloadTags) {
		return ChildBuffer{}, ErrPayloadUnreadable
	}
	return ChildBuffer{Intent: sg.Intent, Payload: sg.Payload}, nil
}
