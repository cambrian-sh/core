package agentplane

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AgentDialer boots agent instances and provides gRPC connections.
// Defined at the consumer side so AgentManager can be mocked in tests.
type AgentDialer interface {
	GetOrBootInstance(ctx context.Context, def *domain.AgentDefinition, excludeInstanceID string) *domain.Instance
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
	inst := a.Manager.GetOrBootInstance(ctx, &agent, excludeInstanceID)
	if inst == nil {
		return nil, fmt.Errorf("getOrDialClient: boot agent %s failed", agent.ID)
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

// RequestProposalFrom calls a specific agent with an already-constructed ProposalRequest.
// Implements domain.ProposalRequester.
//
// This is NOT a bid. The auction's bid round is gone; this survives because the
// INTERVIEW worker uses the same RPC to put a scenario to an agent during
// onboarding, which is a different question asked over the same wire.
func (a *Transport) RequestProposalFrom(ctx context.Context, agent domain.AgentDefinition, req domain.ProposalRequest) (domain.ProposalResponse, error) {
	client, err := a.getOrDialClient(ctx, agent, "")
	if err != nil {
		return domain.ProposalResponse{}, fmt.Errorf("RequestProposalFrom: %w", err)
	}
	protoResp, err := client.RequestProposal(ctx, &pb.ProposalRequest{
		TaskId:         req.TaskID,
		Description:    req.Description,
		Context:        req.Context,
		Deadline:       timestamppb.New(req.Deadline),
		ConfidenceHint: req.ConfidenceHint,
	})
	if err != nil {
		return domain.ProposalResponse{}, err
	}
	return domain.ProposalResponse{
		Confidence:         protoResp.Confidence,
		Rationale:          protoResp.Rationale,
		Requirements:       protoResp.Requirements,
		EstimatedLatencyMs: protoResp.EstimatedLatencyMs,
		Metadata:           protoResp.Metadata,
	}, nil
}

// VerifyOutput calls the VerifyOutput RPC on a verifier agent.
// Implements domain.VerifyRequester.
func (a *Transport) VerifyOutput(ctx context.Context, agent domain.AgentDefinition, req domain.VerifyRequest) (domain.VerifyResponse, error) {
	client, err := a.getOrDialClient(ctx, agent, "")
	if err != nil {
		return domain.VerifyResponse{}, fmt.Errorf("VerifyOutput: %w", err)
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

// domainToProtoHandoff converts a domain Handoff to proto for gRPC calls.
func domainToProtoHandoff(d *domain.Handoff) *pb.Handoff {
	if d == nil {
		return nil
	}
	h := &pb.Handoff{
		Id:            d.ID,
		FromAgent:     d.FromAgent,
		ToAgent:       d.ToAgent,
		Confidence:    d.Confidence,
		Uncertainties: d.Uncertainties,
		Metadata:      d.Context,
	}
	if d.Payload != nil {
		h.Payload = &pb.Object{
			Id:       d.Payload.ID,
			Type:     d.Payload.Type,
			Data:     d.Payload.Data,
			Metadata: d.Payload.Metadata,
		}
	}
	for _, ref := range d.WorkingMemory {
		h.WorkingMemory = append(h.WorkingMemory, &pb.ContextRef{
			Cid:        string(ref.CID),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return h
}

// protoToDomainHandoff converts a proto Handoff to domain for internal use.
func protoToDomainHandoff(h *pb.Handoff) *domain.Handoff {
	if h == nil {
		return nil
	}
	d := &domain.Handoff{
		ID:            h.Id,
		FromAgent:     h.FromAgent,
		ToAgent:       h.ToAgent,
		Confidence:    h.Confidence,
		Uncertainties: h.Uncertainties,
		Context:       h.GetMetadata(),
	}
	if h.Payload != nil {
		d.Payload = &domain.Payload{
			ID:       h.Payload.Id,
			Type:     h.Payload.Type,
			Data:     h.Payload.Data,
			Metadata: h.Payload.Metadata,
		}
	}
	for _, ref := range h.WorkingMemory {
		d.WorkingMemory = append(d.WorkingMemory, domain.ContextRef{
			CID:        domain.CID(ref.Cid),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return d
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
	client, err := a.getOrDialClient(ctx, *agent, excludeInstanceID)
	if err != nil {
		return nil, err
	}
	protoResp, err := client.Execute(ctx, domainToProtoHandoff(handoff))
	if err != nil {
		return nil, err
	}
	return protoToDomainHandoff(protoResp), nil
}
