package network

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type recordingGen struct{ called *bool }

func (r recordingGen) Generate(_ context.Context, _ string) (string, error) {
	*r.called = true
	return "ok", nil
}

// wrapGen is identity when no decorator is wired (OSS / Langfuse disabled).
func TestServer_wrapGen_IdentityWhenNil(t *testing.T) {
	s := &Server{}
	inner := recordingGen{called: new(bool)}
	if got := s.wrapGen(inner); got != domain.Generator(inner) {
		t.Error("wrapGen should return the generator unchanged when GenWrapper is nil")
	}
}

// wrapGen applies the decorator so routed thought-step generations are traced.
func TestServer_wrapGen_AppliesDecorator(t *testing.T) {
	wrapped := false
	s := &Server{GenWrapper: func(g domain.Generator) domain.Generator {
		wrapped = true
		return g
	}}
	_ = s.wrapGen(recordingGen{called: new(bool)})
	if !wrapped {
		t.Error("wrapGen should apply GenWrapper when set")
	}
}
