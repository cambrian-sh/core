package app

import (
	"testing"

	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ADR-0150 D3: execution.lane reaches the chat path — the single-agent lane (the
// default) turns the merged conversation agent on with the lane's D4 limits; the
// planner lane leaves the front desk as it was.
func TestWireChatLane_BothValues(t *testing.T) {
	cfg := config.DefaultConfig().Execution
	server := &subnetwork.Server{ExecCfg: cfg}

	turns := corechat.NewTurnService(nil, nil, nil)
	wireChatLane(cfg, turns, server)
	if !turns.Lane.SingleAgent {
		t.Fatal("the default lane did not reach the chat path")
	}
	if l := turns.Lane.Limits; l.MaxToolRounds != 500 || l.MaxWallSeconds != 3600 || l.SameResultRepeats != 4 ||
		l.SameFailureRepeats != cfg.Tools.RepeatedFailureStop {
		t.Fatalf("limits %+v", l)
	}
	if turns.Lane.KeepUserMessagesTokens != 20000 {
		t.Fatalf("keep_user_messages_tokens = %d", turns.Lane.KeepUserMessagesTokens)
	}

	planner := cfg
	planner.Lane.Default = config.LanePlanner
	turns2 := corechat.NewTurnService(nil, nil, nil)
	wireChatLane(planner, turns2, &subnetwork.Server{ExecCfg: planner})
	if turns2.Lane.SingleAgent {
		t.Fatal("the planner lane turned the merged agent on")
	}
}
