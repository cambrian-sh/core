package agentmgr

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 S1 e2e, second half: the SHIPPED agents work under confinement —
// the chat pool's program (agents/chat_agent.py) and one organ
// (agents/system/retrieval_agent) boot through the real launch path with the
// kernel directory hidden, find their prompts/ and skills/ again, open their
// socket in the per-instance socket directory and serve grpc.health.v1.
//
// It needs a Python with the agent SDK and a prepared kernel directory, so it
// runs only when they are named:
//
//	AGENTMGR_E2E_PY      the interpreter (a venv outside the kernel dir is fine)
//	AGENTMGR_E2E_KERNEL  a kernel directory holding agents/, prompts/, skills/
//	                     and configs/{config.db,secret.key}
//
// (the Linux container run in the ENG-1959 log sets both). The live pool and a
// running kernel are ADR-0157 S7.
func TestE2E_ShippedAgentsServeUnderConfinement(t *testing.T) {
	py, kernel := os.Getenv("AGENTMGR_E2E_PY"), os.Getenv("AGENTMGR_E2E_KERNEL")
	if py == "" || kernel == "" {
		t.Skip("set AGENTMGR_E2E_PY and AGENTMGR_E2E_KERNEL to run the shipped agents under the sandbox")
	}
	root := e2eRoot(t)
	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot:         filepath.Join(root, "w"),
		KernelDir:        kernel,
		KernelStatePaths: []string{filepath.Join(kernel, "configs")},
		RuntimeReadPaths: []string{filepath.Join(kernel, "agents"), filepath.Join(kernel, "prompts"), filepath.Join(kernel, "skills")},
	})
	if !slices.Contains(planAgent(sb).Enforced, sandbox.ControlHideState) {
		t.Log("note: no read confinement on this host; the agents still run through the sandbox")
	}
	im := NewInstanceManager(py, "127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetAgentBootTimeout(60 * time.Second)
	for _, def := range []*domain.AgentDefinition{
		{ID: "chat_agent", Runtime: domain.RuntimePython, ExecPath: "chat_agent.py", Dir: filepath.Join(kernel, "agents")},
		{ID: "retrieval_agent", Runtime: domain.RuntimePython, ExecPath: "agent.py",
			Dir: filepath.Join(kernel, "agents", "system", "retrieval_agent"), System: true},
	} {
		t.Run(def.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			inst, err := im.GetOrBootInstance(ctx, def, "")
			if err != nil {
				t.Fatalf("boot under confinement: %v", err)
			}
			defer im.EvictInstance(inst.ID)
			if err := waitHealthy(ctx, inst.SocketPath, 20*time.Second); err != nil {
				t.Fatalf("%s does not serve under confinement: %v", def.ID, err)
			}
			time.Sleep(2 * time.Second)
			im.mu.Lock()
			exited := im.exitedLocked(inst.ID)
			im.mu.Unlock()
			if exited {
				t.Fatalf("%s exited after serving", def.ID)
			}
		})
	}
}
