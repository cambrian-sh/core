package domain

import "testing"

// ENG-1426: the guards classify "sending" through one function (Exercises over
// CeilingEffects + EffectCovers) — the D14 constraint guard, the Rule of Two gate
// and the outbound identifier guard agree on every tool.
func TestExercises_EgressSendIsTheDeclaredClass(t *testing.T) {
	cases := []struct {
		name string
		tool SystemTool
		want bool
	}{
		{"egress_read named send_*", SystemTool{Name: "send_preview", Effects: []ToolEffect{EffectRead, EffectEgressRead}}, false},
		{"egress_send without a send name", SystemTool{Name: "notify_webhook", Effects: []ToolEffect{EffectEgressSend}}, true},
		{"plain egress (may send, fail closed)", SystemTool{Name: "fetch_page", Effects: []ToolEffect{EffectEgress}}, true},
		{"inferred egress counts as send", SystemTool{Name: "fetch_page", Effects: []ToolEffect{EffectRead, EffectEgress}, EffectsInferred: true}, true},
		{"read only", SystemTool{Name: "send_log_reader", Effects: []ToolEffect{EffectRead}}, false},
	}
	gate, err := NewRuleOfTwoGate(RuleOfTwoPolicy{
		UntrustedSources: []SourceClass{SourceWeb}, GatedEffects: []ToolEffect{EffectWrite, EffectEgressSend},
	}, NewProvenanceTracker(0, nil))
	if err != nil {
		t.Fatal(err)
	}
	cons := []ConstraintGuard{{Item: "R1", Quote: "don't send anything", Terms: []string{"send"}, Classes: []ToolEffect{EffectEgressSend}}}
	for _, c := range cases {
		if got := Exercises(c.tool, EffectEgressSend); got != c.want {
			t.Errorf("%s: Exercises(egress_send) = %v, want %v", c.name, got, c.want)
		}
		_, _, forbidden := ForbiddenBy(c.tool, cons)
		gated := len(gate.gatedEffects(c.tool)) > 0
		if forbidden != c.want || gated != c.want || SendsOutside(c.tool) != c.want {
			t.Errorf("%s: constraint=%v rule-of-two=%v outbound=%v, want all %v", c.name, forbidden, gated, SendsOutside(c.tool), c.want)
		}
	}
}

// The broad class still covers both halves: "do not reach outside" (egress)
// refuses an egress_read tool too, and an effect the step's own write item
// carries is never ruled out (ADR-0145 D14.6).
func TestExercisesUncarried_CoversAndCarries(t *testing.T) {
	read := SystemTool{Name: "web_search", Effects: []ToolEffect{EffectEgressRead}}
	if !Exercises(read, EffectEgress) {
		t.Error("egress covers egress_read")
	}
	plain := SystemTool{Name: "mail_send", Effects: []ToolEffect{EffectWrite, EffectEgress}}
	if ExercisesUncarried(plain, EffectEgressSend, []ToolEffect{EffectEgressSend}) {
		t.Error("a carried egress_send must not count against a send constraint")
	}
	if !ExercisesUncarried(plain, EffectEgress, []ToolEffect{EffectEgressSend}) {
		t.Error("plain egress still exercises egress_read, which a carried send does not cover")
	}
	if ExercisesUncarried(plain, EffectEgress, []ToolEffect{EffectEgress}) {
		t.Error("a carried plain egress covers both halves")
	}
	if Exercises(SystemTool{Name: "bare"}, EffectEgressSend) {
		t.Error("a tool with no effects exercises nothing (callers fail closed themselves)")
	}
}
