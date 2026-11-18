package app

import (
	"context"
	"testing"
)

// T-20261118-remaining-context-background-leaks-on-request (ENG-2055): the
// composition-root seams run under the caller's ctx, not context.Background()
// (AGENTS.md §2.7). The archlint rule TestArch_NoBackgroundContextOnRequestPaths
// no longer allowlists them.

type appReqKey struct{}

func appReqCtx(t *testing.T) context.Context {
	return context.WithValue(t.Context(), appReqKey{}, "rpc")
}

type recordingEffects struct {
	got    context.Context
	params map[string]float64
}

func (r *recordingEffects) SetRuntimeConfig(ctx context.Context, params map[string]float64) error {
	r.got, r.params = ctx, params
	return nil
}

// SetConfig → setOne → hotApplyFor's hook → SetRuntimeConfig carries the request
// ctx end to end, through the real config write source and store.
func TestSetConfig_HotApplyRunsUnderTheRequestContext(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	eff := &recordingEffects{}
	src.hotApply = hotApplyFor(eff)

	if _, err := src.SetConfig(appReqCtx(t), map[string]float64{"execution.blend_weight_cosine": 0.6}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if eff.got == nil || eff.got.Value(appReqKey{}) != "rpc" {
		t.Fatal("SetRuntimeConfig must run under the SetConfig request's ctx")
	}
	if eff.params["blend_weight_cosine"] != 0.6 {
		t.Fatalf("hot-apply params = %v", eff.params)
	}
}

func TestPlanSubmitter_KnownAgentPassesTheContext(t *testing.T) {
	var got context.Context
	p := planSubmitter{known: func(ctx context.Context, id string) bool { got = ctx; return id == "chat_agent" }}
	if !p.KnownAgent(appReqCtx(t), "chat_agent") {
		t.Fatal("KnownAgent must answer from the lookup")
	}
	if got == nil || got.Value(appReqKey{}) != "rpc" {
		t.Fatal("the pin lookup must run under the caller's ctx")
	}
}
