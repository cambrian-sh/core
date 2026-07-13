package executer

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — same inputs always produce the same key.
func TestStepCacheKey_Deterministic(t *testing.T) {
	step := domain.Step{Query: "summarise Q3 report", DependsOn: []int{0, 1}}
	snapshot := map[string]string{
		"step_0_result": "revenue data",
		"step_1_result": "cost data",
	}
	k1 := stepCacheKey("finance plan", "any-plan-id", step, snapshot)
	k2 := stepCacheKey("finance plan", "any-plan-id", step, snapshot)
	if k1 != k2 {
		t.Errorf("expected deterministic key, got %q and %q", k1, k2)
	}
	if k1 == "" {
		t.Error("key must not be empty")
	}
}

// Cycle 2 — root steps (no DependsOn) are salted by planID.
func TestStepCacheKey_RootStep_SaltedByPlanID(t *testing.T) {
	step := domain.Step{Query: "read data/users.csv", DependsOn: nil}
	snapshot := map[string]string{}

	k1 := stepCacheKey("plan", "planid-aaa", step, snapshot)
	k2 := stepCacheKey("plan", "planid-bbb", step, snapshot)
	if k1 == k2 {
		t.Error("root step keys with different planIDs must differ to prevent cross-invocation collision")
	}
}

// Cycle 3 — dependent steps with same inputs produce the same key regardless of planID.
func TestStepCacheKey_DependentStep_NotSaltedByPlanID(t *testing.T) {
	step := domain.Step{Query: "summarise", DependsOn: []int{0}}
	snapshot := map[string]string{"step_0_result": "some upstream result"}

	k1 := stepCacheKey("plan", "planid-aaa", step, snapshot)
	k2 := stepCacheKey("plan", "planid-bbb", step, snapshot)
	if k1 != k2 {
		t.Errorf("dependent step keys must NOT include planID salt (cross-plan reuse): got %q and %q", k1, k2)
	}
}

// Cycle 4 — different dep outputs produce different keys.
func TestStepCacheKey_DifferentDepOutputs_DifferentKey(t *testing.T) {
	step := domain.Step{Query: "summarise", DependsOn: []int{0}}

	k1 := stepCacheKey("plan", "pid", step, map[string]string{"step_0_result": "result A"})
	k2 := stepCacheKey("plan", "pid", step, map[string]string{"step_0_result": "result B"})
	if k1 == k2 {
		t.Error("different dependency outputs must produce different cache keys")
	}
}
