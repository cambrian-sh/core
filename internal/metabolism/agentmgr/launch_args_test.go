package agentmgr

import (
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// A profile agent is the one runtime launched with its profile and its registry id
// (agent stack v2): the per-agent LaunchArgs follow the kernel's own flags.
func TestBuildAgentCmd_AppendsLaunchArgs(t *testing.T) {
	im := NewInstanceManager("python", "localhost:50051")
	def := &domain.AgentDefinition{
		ID: "data-analyst", Runtime: domain.RuntimePython, ExecPath: "worker_agent.py", Dir: "/agents",
		LaunchArgs: []string{"--profile", "data-analyst", "--agent-id", "data-analyst"},
	}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "i1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[1] != "worker_agent.py" {
		t.Fatalf("argv[1] = %q", cmd.Args[1])
	}
	i := slices.Index(cmd.Args, "--profile")
	if i < 0 || cmd.Args[i+1] != "data-analyst" || cmd.Args[i+2] != "--agent-id" || cmd.Args[i+3] != "data-analyst" {
		t.Fatalf("launch args missing or out of order: %v", cmd.Args)
	}
	if slices.Index(cmd.Args, "--substrate-addr") > i {
		t.Fatalf("launch args must follow the kernel's flags: %v", cmd.Args)
	}

	plain := &domain.AgentDefinition{ID: "chat_agent", Runtime: domain.RuntimePython, ExecPath: "chat_agent.py", Dir: "/agents"}
	cmd, err = im.buildAgentCmd(plain, &domain.Instance{ID: "i2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "--profile") || slices.Contains(cmd.Args, "--agent-id") {
		t.Fatalf("an agent without launch args got some: %v", cmd.Args)
	}
}
