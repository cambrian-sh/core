package agentmgr

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0130 D1: the spawned agent's environment carries ITS credential and id,
// and nothing else credential-shaped — SEC-01's filter still strips the
// kernel's own secrets from the inherited environment.
func TestBuildAgentCmd_InjectsOnlyTheAgentsOwnCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "kernel-secret-must-not-leak")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "also-must-not-leak")
	im := NewInstanceManager("python", "localhost:0")
	im.SetAgentCredentialSource(func(id string) (string, bool) {
		if id == "analyst_agent" {
			return "tok-analyst", true
		}
		return "", false
	})
	def := &domain.AgentDefinition{ID: "analyst_agent", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "inst-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, AgentTokenEnv+"=tok-analyst") || !strings.Contains(env, AgentIDEnv+"=analyst_agent") {
		t.Fatalf("credential not injected:\n%s", env)
	}
	for _, leak := range []string{"kernel-secret-must-not-leak", "also-must-not-leak"} {
		if strings.Contains(env, leak) {
			t.Fatalf("kernel secret leaked into the agent environment:\n%s", env)
		}
	}

	// An agent with no issued credential spawns without one — no empty token.
	other := &domain.AgentDefinition{ID: "pulse_agent", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err = im.buildAgentCmd(other, &domain.Instance{ID: "inst-2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(cmd.Env, "\n"); strings.Contains(env, AgentTokenEnv) || strings.Contains(env, AgentIDEnv) {
		t.Fatalf("unissued agent received credential variables:\n%s", env)
	}
}

// D4: the kernel-side dial picks TLS for TCP when a root is configured,
// plaintext for unix sockets always, and never errors on a unix address.
func TestAgentConnector_TransportSelection(t *testing.T) {
	plain := &AgentConnector{}
	if _, err := plain.transportCredentials("unix:///tmp/a.sock"); err != nil {
		t.Fatalf("unix dial errored: %v", err)
	}
	if creds, err := plain.transportCredentials("127.0.0.1:50052"); err != nil || creds.Info().SecurityProtocol != "insecure" {
		t.Fatalf("plaintext TCP without a root: creds=%v err=%v", creds, err)
	}
	withRoot := &AgentConnector{tlsCA: "/nonexistent/root.pem"}
	if _, err := withRoot.transportCredentials("127.0.0.1:50052"); err == nil {
		t.Fatal("an unreadable TLS root must fail the dial, not degrade to plaintext")
	}
	if _, err := withRoot.transportCredentials("unix:///tmp/a.sock"); err != nil {
		t.Fatalf("unix dial must ignore the TLS root: %v", err)
	}
}
