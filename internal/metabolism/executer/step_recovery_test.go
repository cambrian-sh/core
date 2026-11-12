package executer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The replan loop guard refuses a replan that restates the failed step. After a
// capability_missing failure the re-planner is asked to re-plan THAT step with
// what it lacked (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under): same
// words, changed requirements, is a re-routing, not a restatement.
func TestReroutedForCapability(t *testing.T) {
	failed := domain.Step{Query: "write the CSV", RequiredCapabilities: []string{"data_analysis"}}
	cm := fmt.Errorf("self-healer: %w", &domain.CapabilityMissingError{Needed: []string{"write a file"}})

	if !reroutedForCapability(cm, failed, domain.Step{Query: "write the CSV", RequiredCapabilities: []string{"file_write"}}) {
		t.Fatal("changed capabilities after capability_missing must pass the guard")
	}
	if !reroutedForCapability(cm, failed, domain.Step{Query: "write the CSV", RequiredCapabilities: []string{"data_analysis"},
		RequiredEffects: []string{"write"}}) {
		t.Fatal("changed effects after capability_missing must pass the guard")
	}
	if reroutedForCapability(cm, failed, domain.Step{Query: "write the CSV", RequiredCapabilities: []string{"Data-Analysis"}}) {
		t.Fatal("an unchanged requirement set is a restatement")
	}
	if reroutedForCapability(errors.New("agent failed"), failed, domain.Step{Query: "write the CSV", RequiredCapabilities: []string{"file_write"}}) {
		t.Fatal("only a capability_missing failure licenses the same words")
	}
}
