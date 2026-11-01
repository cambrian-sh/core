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

// ADR-0130 D4 (T-20261101-kernel-spawned-agents-dial-localhost-port-and): under
// a TLS transport the spawned agent is told the reachable address and the TLS
// root (and server-name override), next to its credential.
func TestBuildAgentCmd_TLSTransportReachesTheAgent(t *testing.T) {
	im := NewInstanceManager("python", "localhost:50051")
	im.SetAgentTransport(AgentTransport{Addr: "192.168.1.10:50051", TLSCAFile: "/etc/cambrian/tls.crt", TLSServerName: "kernel.lan"})
	def := &domain.AgentDefinition{ID: "analyst_agent", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "inst-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--substrate-addr 192.168.1.10:50051") {
		t.Fatalf("agent not given the reachable address: %s", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, TLSCAEnv+"=/etc/cambrian/tls.crt") || !strings.Contains(env, TLSServerNameEnv+"=kernel.lan") {
		t.Fatalf("TLS root/server name not injected:\n%s", env)
	}
}

// Plaintext (loopback) transport injects no TLS variables, and an empty Addr
// keeps the constructed address.
func TestBuildAgentCmd_PlaintextTransportInjectsNoTLS(t *testing.T) {
	im := NewInstanceManager("python", "localhost:50051")
	im.SetAgentTransport(AgentTransport{TLSServerName: "ignored-without-a-root"})
	def := &domain.AgentDefinition{ID: "a", Runtime: domain.RuntimeBinary, ExecPath: "agent"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "inst-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(cmd.Args, " "); !strings.Contains(args, "--substrate-addr localhost:50051") {
		t.Fatalf("constructed address lost: %s", args)
	}
	if env := strings.Join(cmd.Env, "\n"); strings.Contains(env, TLSCAEnv) || strings.Contains(env, TLSServerNameEnv) {
		t.Fatalf("plaintext transport injected TLS variables:\n%s", env)
	}
}
