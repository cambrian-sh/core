package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// ENG-2246: the SubmitPlan seam reports the kernel's lane, so the operator plane
// says when an authored plan will run as one brief (operator.OneBriefRunner).
func TestPlanSubmitterFor_ReportsTheSingleAgentLane(t *testing.T) {
	for lane, want := range map[string]bool{config.LaneSingleAgent: true, config.LanePlanner: false} {
		k := &Kernel{SessionMgr: session.New(nil),
			Server: &subnetwork.Server{ExecCfg: config.ExecutionConfig{Lane: config.LaneConfig{Default: lane}}}}
		b, ok := planSubmitterFor(k, nil).(operator.OneBriefRunner)
		if !ok || b.RunsAsOneBrief() != want {
			t.Fatalf("lane %s: OneBriefRunner=%v RunsAsOneBrief=%v, want %v", lane, ok, ok && b.RunsAsOneBrief(), want)
		}
	}
}
