package agentmgr

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/internal/metabolism/connector"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentTLSCAEnv names a PEM root the kernel verifies an agent's TLS server
// against when it dials a TCP agent address (ADR-0130 D4). Unset ⇒ TCP agent
// dials are plaintext and say so once. Unix-socket dials never use TLS: the
// socket is the process boundary, and that is the shipped shape
// (InstanceManager.socketPath).
const AgentTLSCAEnv = "CAMBRIAN_AGENT_TLS_CA"

// AgentConnector manages gRPC connections and A2A protocol clients for agent
// sessions. It used to also hold CoW git snapshots of agent dirs (ADR-0005
// D2b); those were removed by owner decision 2026-10-31.
type AgentConnector struct {
	a2aClient *connector.A2AClient
	// tlsCA is the PEM root for TCP agent dials; empty means plaintext.
	tlsCA string
	// warnOnce keeps the insecure-dial warning to one line per connector: a
	// fleet of local agents over UDS is not a finding, and a WARN per dial
	// would teach operators to ignore the one that matters.
	warnOnce sync.Once
}

// NewAgentConnector creates an AgentConnector with default wiring. The TLS
// root comes from the environment (AgentTLSCAEnv) rather than config because
// the agent manager is built inside the metabolism stack from a config block
// that must not grow a field nothing reads (AGENTS.md §3.4); the env var is
// read once, here.
func NewAgentConnector() *AgentConnector {
	return &AgentConnector{
		a2aClient: connector.New(),
		tlsCA:     strings.TrimSpace(os.Getenv(AgentTLSCAEnv)),
	}
}

func (ac *AgentConnector) getA2AClient() *connector.A2AClient { return ac.a2aClient }

// DialAgent connects to the specified agent via its address.
//
// ADR-0130 D4: a TCP address is dialed over TLS when AgentTLSCAEnv names a
// root, and in plaintext (warned once) otherwise. A unix: address is always
// plaintext — the socket's filesystem permissions are the boundary.
func (ac *AgentConnector) DialAgent(addr string) (*grpc.ClientConn, error) {
	creds, err := ac.transportCredentials(addr)
	if err != nil {
		return nil, err
	}
	// Use grpc.NewClient (non-blocking) instead of the deprecated grpc.Dial
	// with WithBlock. Blocking dials can hang indefinitely on Windows UDS
	// when the agent socket exists but the gRPC server is not yet serving.
	// NewClient defers connection establishment to the first RPC, which
	// respects the RPC's context deadline.
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

// transportCredentials picks the credential for one address.
func (ac *AgentConnector) transportCredentials(addr string) (credentials.TransportCredentials, error) {
	if isUnixAddr(addr) {
		return insecure.NewCredentials(), nil
	}
	if ac.tlsCA != "" {
		creds, err := credentials.NewClientTLSFromFile(ac.tlsCA, "")
		if err != nil {
			return nil, fmt.Errorf("agent connector: load TLS root %s=%q: %w", AgentTLSCAEnv, ac.tlsCA, err)
		}
		return creds, nil
	}
	ac.warnOnce.Do(func() {
		slog.Warn("ADR-0130: dialing agents over TCP in PLAINTEXT",
			"addr", addr, "fix", "set "+AgentTLSCAEnv+" to the PEM root the agents' TLS servers chain to")
	})
	return insecure.NewCredentials(), nil
}

// isUnixAddr reports whether a gRPC target names a unix domain socket.
func isUnixAddr(addr string) bool {
	return strings.HasPrefix(addr, "unix:") || strings.HasPrefix(addr, "unix-abstract:")
}
