package centralexec

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// A re-plan is minimal and forward-only (0037-06 #3): only the failed step and
// its transitive downstream dependents are re-planned; still-valid completed
// results (independent or upstream steps) are preserved — never a restart.
func TestForwardOnlyReplanScope(t *testing.T) {
	steps := []domain.Step{
		{Query: "retrieve"},                      // 0
		{Query: "analyze", DependsOn: []int{0}},  // 1  <- fails
		{Query: "summarize", DependsOn: []int{1}}, // 2  depends on 1 (downstream)
		{Query: "sidebar", DependsOn: []int{0}},   // 3  independent of 1 (preserved)
	}

	scope := ForwardOnlyReplanScope(steps, 1)
	want := []int{1, 2}
	if !reflect.DeepEqual(scope, want) {
		t.Errorf("ForwardOnlyReplanScope = %v, want %v (step 0 + 3 preserved)", scope, want)
	}
}

// The escalation ladder is a deterministic state machine (ADR-0037 D3): the
// rung is fixed control flow, only its content is inference. These cases pin the
// ordering re-bind → re-frame → re-plan → fail and the load-bearing invariant
// that a soft/degraded failure never re-plans (0037-06).
func TestEscalate_RungOrdering(t *testing.T) {
	tests := []struct {
		name    string
		failure StepFailure
		state   LadderState
		want    EscalationAction
	}{
		{
			name:    "re-bind first when an alternative resource remains",
			failure: StepFailure{Hard: true, AlternativeResourcesRemain: true, Reframable: true, DownstreamDependencyInvalidated: true},
			state:   LadderState{MaxRebinds: 2, ReframeBudget: 1},
			want:    ActionReBind,
		},
		{
			name:    "re-frame when re-bind is exhausted but the intent is reframable",
			failure: StepFailure{Hard: true, AlternativeResourcesRemain: false, Reframable: true, DownstreamDependencyInvalidated: true},
			state:   LadderState{MaxRebinds: 2, RebindAttempts: 2, ReframeBudget: 1},
			want:    ActionReFrame,
		},
		{
			name:    "re-plan only on a hard failure that invalidated a downstream dependency",
			failure: StepFailure{Hard: true, AlternativeResourcesRemain: false, Reframable: false, DownstreamDependencyInvalidated: true},
			state:   LadderState{MaxRebinds: 2, RebindAttempts: 2, ReframeBudget: 0},
			want:    ActionRePlan,
		},
		{
			name:    "soft/degraded failure never re-plans — it fails honestly, not structurally",
			failure: StepFailure{Hard: false, AlternativeResourcesRemain: false, Reframable: false, DownstreamDependencyInvalidated: true},
			state:   LadderState{MaxRebinds: 2, RebindAttempts: 2, ReframeBudget: 0},
			want:    ActionFail,
		},
		{
			name:    "re-frame is skipped when the D15 progress guard trips (livelock signature)",
			failure: StepFailure{Hard: true, AlternativeResourcesRemain: false, Reframable: true, DownstreamDependencyInvalidated: true},
			state:   LadderState{MaxRebinds: 2, RebindAttempts: 2, ReframeBudget: 3, ProgressStalled: true},
			want:    ActionRePlan,
		},
		{
			name:    "budget exhaustion with no hard-downstream invalidation fails (capability/budget terminus)",
			failure: StepFailure{Hard: true, AlternativeResourcesRemain: false, Reframable: true, DownstreamDependencyInvalidated: false},
			state:   LadderState{MaxRebinds: 2, RebindAttempts: 2, ReframeBudget: 1, ReframeAttempts: 1},
			want:    ActionFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Escalate(tt.failure, tt.state); got != tt.want {
				t.Errorf("Escalate() = %v, want %v", got, tt.want)
			}
		})
	}
}
