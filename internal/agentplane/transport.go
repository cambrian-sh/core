package agentplane

import (
	"context"
	"fmt"
	"github.com/cambrian-sh/core/internal/agentwire"
	"sync"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AgentDialer boots agent instances and provides gRPC connections.
// Defined at the consumer side so AgentManager can be mocked in tests.
type AgentDialer interface {
	GetOrBootInstance(ctx context.Context, def *domain.AgentDefinition, excludeInstanceID string) (*domain.Instance, error)
	DialAgent(addr string) (*grpc.ClientConn, error)
	GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error)
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

type Transport struct {
	agentClients map[string]pb.AgentServiceClient
	agentConns   map[string]*grpc.ClientConn
	mu           sync.RWMutex
	// dialGroup single-flights the boot+dial+register per agent so concurrent
	// cache-misses share ONE connection instead of each dialing a fresh conn and
	// racing RegisterAgentClient's oldConn.Close() (which fails the loser's
	// in-flight RPC with "the client connection is closing"). Zero value is ready.
	dialGroup singleflight.Group
	Manager   AgentDialer

	// ContextLimits are the context-engine settings (execution.lane.context,
	// ADR-0150 D10) stamped on every handoff to a kernel-spawned agent; set by the
	// composition root (kernel.NewMetabolismStack). Zero stamps nothing.
	ContextLimits domain.AgentContextLimits

	// CallAgentHook, when non-nil, replaces callAgent for testing.
	CallAgentHook func(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error)
}

// New creates the agent connection pool + invocation primitive.
func New(manager AgentDialer) *Transport {
	return &Transport{
		agentClients: make(map[string]pb.AgentServiceClient),
		agentConns:   make(map[string]*grpc.ClientConn),
		Manager:      manager,
	}
}

func (a *Transport) getAgentClient(agentID string) (pb.AgentServiceClient, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	client, ok := a.agentClients[agentID]
	if !ok {
		return nil, fmt.Errorf("agent client not found: %s", agentID)
	}
	return client, nil
}

// getOrDialClient returns a connected gRPC client for the agent, booting a new
// Instance via UDS if none is currently tracked.
func (a *Transport) getOrDialClient(ctx context.Context, agent domain.AgentDefinition, excludeInstanceID string) (pb.AgentServiceClient, error) {
	// excludeInstanceID set ⇒ the caller explicitly wants a fresh instance
	// (retry/exclude); bypass the cache and the single-flight key (which is the
	// agent ID, and would wrongly merge distinct excludes).
	if excludeInstanceID != "" {
		return a.bootDialRegister(ctx, agent, excludeInstanceID)
	}

	if client, err := a.getAgentClient(agent.ID); err == nil {
		return client, nil
	}

	// Single-flight per agent: concurrent cache-misses run the dial ONCE and share
	// the result, so two goroutines never each dial a conn and race
	// RegisterAgentClient's oldConn.Close() (the "client connection is closing"
	// bug surfaced by the EFE path's extra concurrent CallAgent caller).
	v, err, _ := a.dialGroup.Do(agent.ID, func() (interface{}, error) {
		// Re-check under the flight: a prior leader may have just populated the cache.
		if client, cerr := a.getAgentClient(agent.ID); cerr == nil {
			return client, nil
		}
		return a.bootDialRegister(ctx, agent, "")
	})
	if err != nil {
		return nil, err
	}
	return v.(pb.AgentServiceClient), nil
}

// bootDialRegister boots (or reuses) an instance, dials it, and caches the client.
func (a *Transport) bootDialRegister(ctx context.Context, agent domain.AgentDefinition, excludeInstanceID string) (pb.AgentServiceClient, error) {
	inst, err := a.Manager.GetOrBootInstance(ctx, &agent, excludeInstanceID)
	if err != nil {
		return nil, fmt.Errorf("getOrDialClient: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("getOrDialClient: boot agent %s returned no instance", agent.ID)
	}

	addr := "unix:" + inst.SocketPath
	conn, dialErr := a.Manager.DialAgent(addr)
	if dialErr != nil {
		return nil, fmt.Errorf("getOrDialClient: dial agent %s: %w", agent.ID, dialErr)
	}

	client := pb.NewAgentServiceClient(conn)
	a.RegisterAgentClient(agent.ID, client, conn)
	return client, nil
}

// RegisterAgentClient registers a gRPC client and its underlying connection.
func (a *Transport) RegisterAgentClient(agentID string, client pb.AgentServiceClient, conn *grpc.ClientConn) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if oldConn, ok := a.agentConns[agentID]; ok {
		_ = oldConn.Close()
	}

	a.agentClients[agentID] = client
	a.agentConns[agentID] = conn
}

// The kernel-side RequestProposal caller (RequestProposalFrom) was removed
// 2026-11-05 with the LLM agent interview, its last caller
// (T-20261105-delete-the-llm-agent-interview-now-owner). The auction's bid round was
// retired earlier (ADR-0100). AgentService.RequestProposal stays in the proto,
// deprecated, so agents that still serve it remain wire-compatible.

// VerifyOutput calls the VerifyOutput RPC on a verifier agent.
// Implements domain.VerifyRequester.
func (a *Transport) VerifyOutput(ctx context.Context, agent domain.AgentDefinition, req domain.VerifyRequest) (domain.VerifyResponse, error) {
	client, err := a.getOrDialClient(ctx, agent, "")
	if err != nil {
		return domain.VerifyResponse{}, fmt.Errorf("VerifyOutput: %w", err)
	}
	// The verifier's lease rides as gRPC metadata (pb.VerifyRequest has no lease
	// field): the agent reads it from its invocation metadata and presents it
	// back on GenerateViaModelStream / GenerateWithTools (ADR-0131;
	// T-20261101-verification-mints-no-lease-so-verifier-llm).
	if req.LeaseID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, domain.LeaseHeader, string(req.LeaseID))
	}
	protoResp, err := client.VerifyOutput(ctx, &pb.VerifyRequest{
		TaskId:        req.TaskID,
		OriginalQuery: req.OriginalQuery,
		WinnerOutput:  req.WinnerOutput,
		WinnerAgentId: req.WinnerAgentID,
		BidConfidence: req.BidConfidence,
	})
	if err != nil {
		return domain.VerifyResponse{}, err
	}
	return domain.VerifyResponse{
		QualityScore: protoResp.QualityScore,
		Critique:     protoResp.Critique,
	}, nil
}

// CallAgent wraps the gRPC Execute call against an agent.
// Converts domain.Handoff ↔ pb.Handoff at the gRPC boundary.
func (a *Transport) CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
	if a.CallAgentHook != nil {
		return a.CallAgentHook(ctx, agentID, handoff, excludeInstanceID)
	}
	agent, err := a.Manager.GetAgentByName(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if agent.Runtime == domain.RuntimeA2A {
		// A remote A2A agent has no local instance to boot or dial: the boot path
		// has no a2a case and refused it, so an A2A agent was unreachable through
		// the Transport (T-20261113-agentmanager-callagent-kills-the-shared-jit).
		if ex, ok := a.Manager.(a2aExecutor); ok {
			return ex.ExecuteA2A(ctx, *agent, handoff)
		}
		return nil, fmt.Errorf("agent %s: a2a runtime is not supported by this dialer", agent.ID)
	}
	// A local, kernel-spawned agent: its context-engine settings ride the handoff
	// (not a remote A2A agent above, whose engine is not ours to tune).
	handoff = a.withContextLimits(handoff)
	if domain.DedicatedInstanceRequested(ctx) {
		if booter, ok := a.Manager.(dedicatedBooter); ok {
			return a.callDedicated(ctx, booter, *agent, handoff)
		}
	}
	client, err := a.getOrDialClient(ctx, *agent, excludeInstanceID)
	if err != nil {
		return nil, err
	}
	protoResp, err := client.Execute(ctx, agentwire.ToProto(handoff))
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			// The cached connection points at an agent that is gone (crashed or
			// killed). Drop it so the next call goes back through
			// GetOrBootInstance, which replaces an exited instance; without this a
			// dead agent stayed unreachable for the life of the kernel
			// (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
			a.forgetAgentClient(agent.ID, client)
		}
		return nil, err
	}
	return agentwire.FromProto(protoResp), nil
}

// withContextLimits returns the handoff with ContextLimits stamped on a copy of
// its context (the caller's handoff is not mutated: a retry or a runner-up reuses
// it). Owner decision 2026-11-14 (T-20261105-context-engine-budgets-cannot-be-
// tuned-for): procenv strips CAMBRIAN_* from a spawned agent's inherited env and a
// profile has no context field, so the handoff is the channel.
func (a *Transport) withContextLimits(h *domain.Handoff) *domain.Handoff {
	if h == nil || a.ContextLimits.IsZero() {
		return h
	}
	cp := *h
	cp.Context = make(map[string]string, len(h.Context)+4)
	for k, v := range h.Context {
		cp.Context[k] = v
	}
	a.ContextLimits.Stamp(cp.Context)
	return &cp
}

// a2aExecutor is the optional AgentDialer surface that serves a RuntimeA2A agent
// (agentmgr.AgentManager implements it).
type a2aExecutor interface {
	ExecuteA2A(ctx context.Context, def domain.AgentDefinition, handoff *domain.Handoff) (*domain.Handoff, error)
}

// dedicatedBooter is the optional AgentDialer surface behind
// domain.WithDedicatedInstance (agentmgr.AgentManager implements it). A dialer
// without it serves dedicated requests on the shared instance, as before.
type dedicatedBooter interface {
	BootDedicatedInstance(ctx context.Context, def *domain.AgentDefinition) (*domain.Instance, error)
	EvictInstance(instanceID string)
}

// callDedicated serves one handoff on a fresh instance booted for it (ADR-0139
// D12: a subagent child must not queue behind its single-threaded parent on the
// shared instance). The connection is NOT cached — RegisterAgentClient would
// close the shared instance's connection under the parent's in-flight call —
// and the instance is evicted (its process killed) when the call returns, which
// is also how cancelling the call stops the child.
func (a *Transport) callDedicated(ctx context.Context, booter dedicatedBooter, agent domain.AgentDefinition, handoff *domain.Handoff) (*domain.Handoff, error) {
	inst, err := booter.BootDedicatedInstance(ctx, &agent)
	if err != nil {
		return nil, fmt.Errorf("dedicated instance: %w", err)
	}
	defer booter.EvictInstance(inst.ID)
	conn, err := a.Manager.DialAgent("unix:" + inst.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("dedicated instance: dial agent %s: %w", agent.ID, err)
	}
	defer func() { _ = conn.Close() }()
	protoResp, err := pb.NewAgentServiceClient(conn).Execute(ctx, agentwire.ToProto(handoff))
	if err != nil {
		return nil, err
	}
	return agentwire.FromProto(protoResp), nil
}

// forgetAgentClient removes agentID's cached client if it is still client (a
// concurrent caller may already have replaced it) and closes its connection.
func (a *Transport) forgetAgentClient(agentID string, client pb.AgentServiceClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.agentClients[agentID]; !ok || cur != client {
		return
	}
	if conn, ok := a.agentConns[agentID]; ok && conn != nil {
		_ = conn.Close() // best-effort: the peer is already gone
	}
	delete(a.agentClients, agentID)
	delete(a.agentConns, agentID)
}
