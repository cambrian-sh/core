package auctioneer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"

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

// GatekeeperProfileReader is the narrow read-only interface used to compute
// ConfidenceHint for proposal requests.
type GatekeeperProfileReader interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// Auctioneer manages the bidding process for a specific task.
// It collects proposals from candidate agents and selects the best one.
type Auctioneer struct {
	agentClients map[string]pb.AgentServiceClient
	agentConns   map[string]*grpc.ClientConn
	mu           sync.RWMutex
	// dialGroup single-flights the boot+dial+register per agent so concurrent
	// cache-misses share ONE connection instead of each dialing a fresh conn and
	// racing RegisterAgentClient's oldConn.Close() (which fails the loser's
	// in-flight RPC with "the client connection is closing"). Zero value is ready.
	dialGroup singleflight.Group
	Manager   AgentDialer
	Gatekeeper   domain.Gatekeeper

	// Profiles is an optional profile reader used to compute ConfidenceHint.
	// When nil, all hints default to 0.0 (cold-start).
	Profiles GatekeeperProfileReader

	MinAuctionConfidence float64
	ExplorationRate      float64
	ExecCfg              config.ExecutionConfig
	EventBus             domain.EventBus          // may be nil; emits auction events internally
	Observer             domain.TelemetryObserver // ADR-0019: may be nil

	// CallAgentHook, when non-nil, replaces callAgent for testing.
	CallAgentHook func(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error)

	// RequestProposalHook, when non-nil, replaces requestProposalFromAgent for testing.
	RequestProposalHook func(ctx context.Context, agent domain.AgentDefinition, task *domain.AuctionTask, confidenceHint float32) (*domain.AgentProposal, error)
}

// NewAuctioneer creates a wired Auctioneer.
func New(manager AgentDialer, gatekeeper domain.Gatekeeper, execCfg config.ExecutionConfig) *Auctioneer {
	return &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		agentConns:           make(map[string]*grpc.ClientConn),
		Manager:              manager,
		Gatekeeper:           gatekeeper,
		MinAuctionConfidence: execCfg.MinAuctionConfidence,
		ExecCfg:              execCfg,
	}
}


// computeConfidenceHint looks up the agent's TrustScore from the profile reader
// and returns a float32 clamped to [0.0, 1.0].
// ADR-0023 Routing Fix: Tool agents receive a neutral 0.0 hint so their Python
// on_proposal handler controls the bid based on task-keyword matching.
// Previously TrustScore=1.0 for all tool agents caused max(hint, fallback)=1.0
// always, making them win every auction regardless of task relevance.
func computeConfidenceHint(ctx context.Context, profiles GatekeeperProfileReader, agent domain.AgentDefinition) float32 {
	if agent.Trait == domain.TraitTool {
		return 0.0
	}
	if profiles == nil {
		return 0.0
	}
	profile, err := profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
	if err != nil || profile == nil {
		return 0.0
	}
	hint := float32(profile.TrustScore)
	if hint < 0.0 {
		hint = 0.0
	}
	if hint > 1.0 {
		hint = 1.0
	}
	return hint
}

func (a *Auctioneer) getAgentClient(agentID string) (pb.AgentServiceClient, error) {
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
func (a *Auctioneer) getOrDialClient(ctx context.Context, agent domain.AgentDefinition, excludeInstanceID string) (pb.AgentServiceClient, error) {
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
func (a *Auctioneer) bootDialRegister(ctx context.Context, agent domain.AgentDefinition, excludeInstanceID string) (pb.AgentServiceClient, error) {
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
func (a *Auctioneer) RegisterAgentClient(agentID string, client pb.AgentServiceClient, conn *grpc.ClientConn) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if oldConn, ok := a.agentConns[agentID]; ok {
		_ = oldConn.Close()
	}

	a.agentClients[agentID] = client
	a.agentConns[agentID] = conn
}

// emitAuctionEvent publishes an AuctionEventPayload on the EventBus if configured.
func (a *Auctioneer) emitAuctionEvent(ev domain.AuctionEventPayload) {
	if a.EventBus == nil {
		return
	}
	_ = a.EventBus.Publish(ev)
}

// ConductAuction broadcasts the RFP to candidates and collects bids.
func (a *Auctioneer) ConductAuction(ctx context.Context, task *domain.AuctionTask, candidates []domain.AgentDefinition) (*domain.AgentProposal, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates found for task %s", task.ID)
	}

	a.emitAuctionEvent(domain.AuctionEventPayload{
		TaskID:   task.ID,
		TaskDesc: task.Description,
		Status:   "started",
	})

	proposalCh := make(chan *domain.AgentProposal, len(candidates))
	var wg sync.WaitGroup

	bidTimeout := time.Duration(a.ExecCfg.AuctionBidTimeoutMs) * time.Millisecond
	bidCtx, cancel := context.WithTimeout(ctx, bidTimeout)
	defer cancel()

	for _, candidate := range candidates {
		wg.Add(1)
		hint := computeConfidenceHint(ctx, a.Profiles, candidate)
		go func(agentInfo domain.AgentDefinition, confidenceHint float32) {
			defer wg.Done()

			proposal, err := a.requestProposalFromAgent(bidCtx, agentInfo, task, confidenceHint)
			if err != nil {
				slog.Warn("bid request failed for agent",
					"agent_id", agentInfo.ID,
					"error", err)
				return
			}
			if proposal != nil {
				proposalCh <- proposal
			}
		}(candidate, hint)
	}

	go func() {
		wg.Wait()
		close(proposalCh)
	}()

	var bestProposal *domain.AgentProposal
	var allBids []domain.BidEntry

	for prop := range proposalCh {
		allBids = append(allBids, domain.BidEntry{
			AgentID:    prop.AgentID,
			Confidence: float32(prop.Confidence),
			Rationale:  prop.Rationale,
			LatencyMs:  int32(prop.Latency),
			IsTool:     prop.IsTool,
		})

		if prop.Confidence < a.MinAuctionConfidence {
			slog.Warn("bid below minimum confidence threshold, discarding",
				"agent_id", prop.AgentID,
				"confidence", prop.Confidence,
				"threshold", a.MinAuctionConfidence,
			)
			continue
		}
		if bestProposal == nil || prop.Confidence > bestProposal.Confidence {
			bestProposal = prop
		}
	}

	if bestProposal == nil {
		a.emitAuctionEvent(domain.AuctionEventPayload{
			TaskID:   task.ID,
			TaskDesc: task.Description,
			Status:   "failed",
			Bids:     allBids,
			ErrorMsg: "no valid proposals received",
		})
		if a.Observer != nil {
			a.Observer.OnAuctionNoWinner(task.ID)
		}
		return nil, fmt.Errorf("no valid proposals received for task %s", task.ID)
	}

	a.emitAuctionEvent(domain.AuctionEventPayload{
		TaskID:   task.ID,
		TaskDesc: task.Description,
		Status:   "completed",
		WinnerID: bestProposal.AgentID,
		Bids:     allBids,
	})

	return bestProposal, nil
}

// VerifyOutput calls the VerifyOutput RPC on a verifier agent.
// Implements domain.VerifyRequester.
func (a *Auctioneer) VerifyOutput(ctx context.Context, agent domain.AgentDefinition, req domain.VerifyRequest) (domain.VerifyResponse, error) {
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

// RequestProposalFrom calls a specific agent with an already-constructed ProposalRequest.
// Implements domain.ProposalRequester.
func (a *Auctioneer) RequestProposalFrom(ctx context.Context, agent domain.AgentDefinition, req domain.ProposalRequest) (domain.ProposalResponse, error) {
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

func (a *Auctioneer) requestProposalFromAgent(ctx context.Context, agent domain.AgentDefinition, task *domain.AuctionTask, confidenceHint float32) (*domain.AgentProposal, error) {
	if a.RequestProposalHook != nil {
		return a.RequestProposalHook(ctx, agent, task, confidenceHint)
	}

	// ADR-0023 Routing Fix: Tool agents no longer bypass the proposal RPC.
	// Their Python on_proposal handlers inspect the task description for
	// keywords and bid 1.0 only when the format matches (e.g. "execute",
	// "run the code" for code_executor; "list", "ls" for terminal).
	// This prevents tool agents from winning auctions on unrelated
	// cognitive tasks simply because they are TraitTool.
	client, err := a.getOrDialClient(ctx, agent, "")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent %s: %w", agent.ID, err)
	}

	protoTask := &pb.ProposalRequest{
		TaskId:         task.ID,
		Description:    task.Description,
		Context:        task.Context,
		Deadline:       timestamppb.New(task.Deadline),
		ConfidenceHint: confidenceHint,
	}

	proposalCtx, cancel := context.WithTimeout(ctx, time.Duration(a.ExecCfg.ProposalTimeoutMs)*time.Millisecond)
	defer cancel()

	resp, err := client.RequestProposal(proposalCtx, protoTask)
	if err != nil {
		return nil, fmt.Errorf("rpc error from agent %s: %w", agent.ID, err)
	}

	return &domain.AgentProposal{
		AgentID:      agent.ID,
		TaskID:       task.ID,
		Confidence:   float64(resp.Confidence),
		Rationale:    resp.Rationale,
		Requirements: resp.Requirements,
		Latency:      int(resp.EstimatedLatencyMs),
		Metadata:     resp.Metadata,
		IsTool:       agent.Trait == domain.TraitTool,
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

// Execute is the high-level entry point for the coordination cycle.
// Returns an AuctionResult with the winning agent's response, confidence, and runner-ups.
func (a *Auctioneer) Execute(ctx context.Context, task *domain.AuctionTask, in *domain.Handoff) (*domain.AuctionResult, error) {
	return a.executeRecursive(ctx, task, in, 0)
}

func (a *Auctioneer) executeRecursive(ctx context.Context, task *domain.AuctionTask, in *domain.Handoff, depth int) (*domain.AuctionResult, error) {
	if depth > a.ExecCfg.MaxRecursionDepth {
		return nil, fmt.Errorf("max recursion depth reached: %d", depth)
	}

	scoredCandidates, err := a.Gatekeeper.FindCandidates(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("gatekeeper failed: %w", err)
	}

	candidates := make([]domain.AgentDefinition, len(scoredCandidates))
	for i, sc := range scoredCandidates {
		candidates[i] = sc.Agent
	}

	if a.ExplorationRate > 0 && len(candidates) > 1 && rand.Float64() < a.ExplorationRate {
		pick := rand.Intn(len(candidates))
		if pick != 0 {
			candidates[0], candidates[pick] = candidates[pick], candidates[0]
		}
		slog.Info("Auctioneer: exploration triggered", "candidates", len(candidates), "picked_idx", pick)
		resp, err := a.CallAgent(ctx, candidates[0].ID, in, "")
		if err != nil {
			return nil, fmt.Errorf("exploration candidate failed: %w", err)
		}
		return &domain.AuctionResult{Handoff: resp, Confidence: 1.0, RunnerUps: scoredCandidates,
			StepAllocation: a.selectModelCandidates(ctx, candidates[0].ID)}, nil
	}

	bestProposal, err := a.ConductAuction(ctx, task, candidates)
	if err != nil {
		return nil, fmt.Errorf("auction failed: %w", err)
	}

	var runnerUps []domain.ScoredCandidate
	for _, sc := range scoredCandidates {
		if sc.Agent.ID != bestProposal.AgentID {
			runnerUps = append(runnerUps, sc)
		}
	}

	if len(bestProposal.Requirements) > 0 {
		workingContext := make(map[string]string)
		for k, v := range in.Context {
			workingContext[k] = v
		}

		for _, req := range bestProposal.Requirements {
			subTask := &domain.AuctionTask{
				ID:              fmt.Sprintf("%s-req-%s", task.ID, req),
				Description:     fmt.Sprintf("Satisfy requirement: %s", req),
				RequiredFormats: []string{req},
			}
			subHandoff := &domain.Handoff{
				Payload: in.Payload,
				Context: workingContext,
			}

			subResult, err := a.executeRecursive(ctx, subTask, subHandoff, depth+1)
			if err != nil {
				return nil, fmt.Errorf("requirement %q failed: %w", req, err)
			}

			key := fmt.Sprintf("requirements.%s.result", req)
			if subResult.Handoff != nil && subResult.Handoff.Payload != nil {
				workingContext[key] = string(subResult.Handoff.Payload.Data)
			}
		}

		in.Context = workingContext
	}

	// ADR-0023 Fix 2: inject the winning capability name so the Python SDK's
	// _dispatch_execute can route to the correct handler without text-matching fallback.
	if in.Context == nil {
		in.Context = make(map[string]string)
	}
	if manifest, mErr := a.Manager.GetManifest(ctx, bestProposal.AgentID); mErr == nil && manifest != nil && len(manifest.Tools) > 0 {
		in.Context["_capability"] = manifest.Tools[0]
	}

	resp, err := a.CallAgent(ctx, bestProposal.AgentID, in, "")
	if err != nil {
		in.Context["_winning_agent_id"] = bestProposal.AgentID
		in.Context["_winning_confidence"] = fmt.Sprintf("%f", bestProposal.Confidence)
		return &domain.AuctionResult{Confidence: bestProposal.Confidence, RunnerUps: runnerUps}, err
	}
	return &domain.AuctionResult{Handoff: resp, Confidence: bestProposal.Confidence, RunnerUps: runnerUps,
		StepAllocation: a.selectModelCandidates(ctx, bestProposal.AgentID)}, nil
}

// selectModelCandidates runs the ADR-0018 TraitModel sub-selection for a step
// whose winning TraitCognitive agent is identified by winnerAgentID.
// Returns nil when no TraitModel agents are registered (backward-compatible).
func (a *Auctioneer) selectModelCandidates(ctx context.Context, winnerAgentID string) *domain.StepAllocation {
	manifest, err := a.Manager.GetManifest(ctx, winnerAgentID)
	if err != nil || manifest == nil {
		return nil
	}
	models, err := a.Gatekeeper.FindModelCandidates(ctx, manifest.RequiredModelCapabilities)
	if err != nil || len(models) == 0 {
		return nil
	}
	sa := &domain.StepAllocation{
		Winner: models[0].Agent,
	}
	if len(models) >= 2 {
		sa.Fallbacks[0] = models[1].Agent
	}
	if len(models) >= 3 {
		sa.Fallbacks[1] = models[2].Agent
	}
	return sa
}

// CallAgent wraps the gRPC Execute call against an agent.
// Converts domain.Handoff ↔ pb.Handoff at the gRPC boundary.
func (a *Auctioneer) CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
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
