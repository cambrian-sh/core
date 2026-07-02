package centralexec

import (
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// A delegated child receives ONLY the explicitly-passed {intent, payload},
// scope-gated to its effective scope (ADR-0037 D11, 0037-08). The parent's other
// working memory is structurally un-representable in a ChildBuffer — leak by
// construction is impossible.
func TestInheritBuffer_DeliversScopedIntentPayloadOnly(t *testing.T) {
	childScope := domain.NewEffectiveScope(
		domain.ScopeConfig{},
		domain.ScopeConfig{AnyOfTags: []string{"public_kb"}},
	)
	sg := SubGoal{
		Intent:  "translate the public notice",
		Payload: &domain.Payload{Type: "text", Data: []byte("hello")},
	}

	buf, err := InheritBuffer(sg, []string{"public_kb"}, &childScope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Intent != "translate the public notice" {
		t.Errorf("Intent = %q, want the sub-goal intent", buf.Intent)
	}
	if buf.Payload == nil || string(buf.Payload.Data) != "hello" {
		t.Error("payload not delivered to the child")
	}
}

// A payload the child is not authorized to read makes the binding FAIL — the CE
// must re-select or escalate, never silently down-scope or leak (D11, #3/#9).
func TestInheritBuffer_UnreadablePayloadFailsBinding(t *testing.T) {
	childScope := domain.NewEffectiveScope(
		domain.ScopeConfig{},
		domain.ScopeConfig{ForbiddenTags: []string{"secret"}},
	)
	sg := SubGoal{Intent: "process the record", Payload: &domain.Payload{Data: []byte("ssn")}}

	_, err := InheritBuffer(sg, []string{"secret"}, &childScope)
	if !errors.Is(err, ErrPayloadUnreadable) {
		t.Errorf("err = %v, want ErrPayloadUnreadable (binding fails, no leak)", err)
	}
}

// A nil child scope is fail-closed — no scope means no read (never leak).
func TestInheritBuffer_NilScopeFailsClosed(t *testing.T) {
	sg := SubGoal{Intent: "x", Payload: &domain.Payload{Data: []byte("y")}}
	if _, err := InheritBuffer(sg, []string{"anything"}, nil); !errors.Is(err, ErrPayloadUnreadable) {
		t.Errorf("nil scope err = %v, want ErrPayloadUnreadable (fail-closed)", err)
	}
}
