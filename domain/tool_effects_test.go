package domain

import (
	"context"
	"errors"
	"testing"
)

func effectSet(es []ToolEffect) map[ToolEffect]bool {
	m := map[ToolEffect]bool{}
	for _, e := range es {
		m[e] = true
	}
	return m
}

// Inference must be PESSIMISTIC: under-classifying a tool is the failure that
// matters, because an under-classified tool slips past a policy that names the
// effect it actually has.
func TestInferEffects_ReadsWhatTheManifestAlreadySays(t *testing.T) {
	cases := []struct {
		name string
		tool SystemTool
		want []ToolEffect
	}{
		{"a plain reader", SystemTool{Name: "web_search"}, []ToolEffect{EffectRead}},
		{"writes a tagged store", SystemTool{Name: "crm_update", DataWriteKinds: []string{"crm"}},
			[]ToolEffect{EffectRead, EffectWrite}},
		{"takes a URL, so it can transmit", SystemTool{Name: "fetch", URLArgs: []string{"url"}},
			[]ToolEffect{EffectRead, EffectEgress}},
		{"runs a shell command", SystemTool{Name: "sh", CommandArgs: []string{"cmd"}},
			[]ToolEffect{EffectRead, EffectWrite}},
		{"the operator already called it dangerous", SystemTool{Name: "rm", Dangerous: true},
			[]ToolEffect{EffectRead, EffectWrite}},
		{"writes and transmits", SystemTool{Name: "webhook", URLArgs: []string{"url"}, DataWriteKinds: []string{"crm"}},
			[]ToolEffect{EffectRead, EffectWrite, EffectEgress}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectSet(InferEffects(tc.tool))
			want := effectSet(tc.want)
			if len(got) != len(want) {
				t.Fatalf("InferEffects = %v, want %v", InferEffects(tc.tool), tc.want)
			}
			for e := range want {
				if !got[e] {
					t.Errorf("missing effect %q in %v", e, InferEffects(tc.tool))
				}
			}
		})
	}
}

// Inference never invents spend or admin — those are claims only a manifest can
// make, and guessing them would be worse than leaving them out.
func TestInferEffects_NeverGuessesSpendOrAdmin(t *testing.T) {
	for _, tool := range []SystemTool{
		{Name: "a", Dangerous: true, URLArgs: []string{"u"}, CommandArgs: []string{"c"}, DataWriteKinds: []string{"x"}},
		{Name: "b"},
	} {
		got := effectSet(InferEffects(tool))
		if got[EffectSpend] || got[EffectAdmin] {
			t.Errorf("inference must not claim spend/admin for %+v, got %v", tool, InferEffects(tool))
		}
	}
}

// A declared set always wins over inference, and is de-duplicated + risk-ordered
// so two tools with the same effects render identically.
func TestValidateRegistration_DeclaredWinsAndIsNormalized(t *testing.T) {
	got, err := ValidateRegistration(SystemTool{
		Name:      "payer",
		Dangerous: true, // would have inferred write
		Effects:   []ToolEffect{EffectSpend, EffectRead, EffectSpend},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectsInferred {
		t.Errorf("a declared set must not be marked inferred")
	}
	if len(got.Effects) != 2 || got.Effects[0] != EffectRead || got.Effects[1] != EffectSpend {
		t.Errorf("expected de-duplicated, risk-ordered [read spend], got %v", got.Effects)
	}
}

// An effect outside the closed set is FATAL in both modes: silently dropping it
// would let a policy that denies it permit the very call it named.
func TestValidateRegistration_UnknownEffectIsAlwaysFatal(t *testing.T) {
	for _, strict := range []bool{false, true} {
		_, err := ValidateRegistration(SystemTool{Name: "x", Effects: []ToolEffect{"delete"}}, strict)
		var unknown *ErrToolUnknownEffect
		if !errors.As(err, &unknown) {
			t.Fatalf("strict=%v: expected ErrToolUnknownEffect, got %v", strict, err)
		}
		if unknown.Effect != "delete" {
			t.Errorf("the error must name the offending effect, got %q", unknown.Effect)
		}
	}
}

// Strict mode refuses to infer — a tool that declares no effects is a
// registration error, not an unrestricted tool.
func TestValidateRegistration_StrictRefusesUndeclared(t *testing.T) {
	_, err := ValidateRegistration(SystemTool{Name: "legacy"}, true)
	var unclassified *ErrToolUnclassified
	if !errors.As(err, &unclassified) {
		t.Fatalf("expected ErrToolUnclassified, got %v", err)
	}
	if unclassified.Tool != "legacy" {
		t.Errorf("the error must name the tool an operator has to fix, got %q", unclassified.Tool)
	}

	// Non-strict accepts it, but marks it so the migration is enumerable.
	got, err := ValidateRegistration(SystemTool{Name: "legacy"}, false)
	if err != nil {
		t.Fatalf("non-strict must accept an un-migrated tool, got %v", err)
	}
	if !got.EffectsInferred || len(got.Effects) == 0 {
		t.Errorf("non-strict must infer and mark, got inferred=%v effects=%v", got.EffectsInferred, got.Effects)
	}
}

// The registry is the chokepoint no path may skip: a tool registered directly
// still comes out classified.
func TestRegistry_NormalizesOnRegister(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file", PathArgs: []string{"path"}})

	got, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("tool should be registered")
	}
	if len(got.Effects) == 0 || !got.EffectsInferred {
		t.Errorf("direct registration must still classify, got %+v", got.Effects)
	}
}

// An invalid effect declaration is refused outright: a downstream "unknown tool"
// is an honest answer, and silently repairing the manifest is not.
func TestRegistry_RefusesInvalidEffectDeclaration(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "bogus", Effects: []ToolEffect{"sudo"}})
	if _, ok := reg.Get("bogus"); ok {
		t.Errorf("a tool with an unrecognisable effect must not reach the executor")
	}
}

// A tool presents its domain to the decision point, falling back to the data
// kinds it already declares so an un-migrated tool is governed by something.
func TestSystemTool_AuthzTags(t *testing.T) {
	explicit := SystemTool{Name: "t", ClassificationTags: []string{"crm"}, DataReadKinds: []string{"ignored"}}
	if got := explicit.AuthzTags(); len(got) != 1 || got[0] != "crm" {
		t.Errorf("explicit classification wins, got %v", got)
	}
	fallback := SystemTool{Name: "t", DataReadKinds: []string{"orders"}, DataWriteKinds: []string{"crm"}}
	if got := fallback.AuthzTags(); len(got) != 2 {
		t.Errorf("expected the data kinds as a fallback, got %v", got)
	}
	if got := (SystemTool{Name: "t"}).AuthzTags(); got != nil {
		t.Errorf("an untagged tool presents no tags, got %v", got)
	}
	if ref := (SystemTool{Name: "t"}).AuthzRef(); ref.Kind != KindTool || ref.ID != "t" {
		t.Errorf("AuthzRef = %+v", ref)
	}
}

// ── the effect gate at execution time ────────────────────────────────────────

// denyEffects is a decision point that refuses one named effect class — the shape
// a sovereign deployment's "nothing leaves this network" rule takes.
type denyEffects struct {
	AllowAllAuthorizer
	deny ToolEffect
}

func (d denyEffects) Authorize(_ context.Context, req AccessRequest) AccessDecision {
	for _, e := range req.Effects {
		if e == d.deny {
			return AccessDecision{
				Allowed: false, Reason: ReasonEffectNotPermitted, Detail: string(e),
				Resource: req.Resource, Principal: req.Principal,
			}
		}
	}
	return AccessDecision{Allowed: true, Reason: ReasonAllowed, Resource: req.Resource}
}

// "No tool may transmit outside this network" — one policy statement, enforced
// over every tool regardless of what data it touches.
func TestToolExecutor_EgressDeniedByEffectPolicy(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "fetch_url", URLArgs: []string{"url"}}) // infers egress
	reg.Register(SystemTool{Name: "read_file", PathArgs: []string{"path"}})

	grants := NewInMemoryGrantsStore()
	grants.Set("a", []ToolGrant{
		{Tool: "fetch_url", Policy: ToolResourcePolicy{AllowAll: true}},
		{Tool: "read_file", Policy: ToolResourcePolicy{AllowAll: true}},
	})
	e := &ToolExecutor{
		Registry: reg, Grants: grants,
		Handler: handlerFunc(func(context.Context, ToolCall) ([]byte, error) { return []byte(`{"ok":true}`), nil }),
		Authz:   denyEffects{deny: EffectEgress},
	}

	resp := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "fetch_url"})
	if !resp.Denied {
		t.Fatalf("an egress tool must be denied under a no-egress policy")
	}
	if resp.DenyReason == "" || !contains(resp.DenyReason, string(EffectEgress)) {
		t.Errorf("the denial must name the effect, got %q", resp.DenyReason)
	}

	// A tool with no egress is unaffected — the rule is about the verb, not the tool.
	if resp := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file"}); resp.Denied {
		t.Errorf("a non-egress tool must still run, got %q", resp.DenyReason)
	}
}

// With no decision point installed the gate still RUNS — it simply always
// permits. That is the OSS posture: no policy, not no check.
func TestToolExecutor_EffectGateRunsButPermitsInOSS(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "fetch_url", URLArgs: []string{"url"}})
	grants := NewInMemoryGrantsStore()
	grants.Set("a", []ToolGrant{{Tool: "fetch_url", Policy: ToolResourcePolicy{AllowAll: true}}})

	e := &ToolExecutor{
		Registry: reg, Grants: grants,
		Handler: handlerFunc(func(context.Context, ToolCall) ([]byte, error) { return []byte(`{"ok":true}`), nil }),
	}
	if resp := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "fetch_url"}); resp.Denied {
		t.Fatalf("an unscoped deployment must permit every effect, got %q", resp.DenyReason)
	}
}

// An operator ScopeSystem execution carries its own authority and is not
// effect-gated — the same carve-out the grant and data-store regimes already make.
func TestToolExecutor_OperatorSystemExecutionSkipsTheEffectGate(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "fetch_url", URLArgs: []string{"url"}})

	e := &ToolExecutor{
		Registry: reg, Grants: NewInMemoryGrantsStore(),
		Handler: handlerFunc(func(context.Context, ToolCall) ([]byte, error) { return []byte(`{"ok":true}`), nil }),
		Authz:   denyEffects{deny: EffectEgress},
	}
	resp := e.Execute(context.Background(), ToolCallRequest{ToolName: "fetch_url", System: true})
	if resp.Denied {
		t.Fatalf("an operator execution must not be effect-gated, got %q", resp.DenyReason)
	}
}

// handlerFunc adapts a func to ToolHandler.
type handlerFunc func(context.Context, ToolCall) ([]byte, error)

func (f handlerFunc) Execute(ctx context.Context, c ToolCall) ([]byte, error) { return f(ctx, c) }
