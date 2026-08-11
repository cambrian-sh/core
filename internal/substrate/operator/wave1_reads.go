package operator

import (
	"context"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// Contract 0072 — the operator console's Wave 1 reads.
//
// Every source here may be nil, and a nil source returns Unimplemented rather
// than an empty success. That distinction is the whole point of several of these
// RPCs: "this deployment has no MCP servers" and "this kernel cannot report MCP
// servers" are different facts, and a console that cannot tell them apart shows
// the same screen for a working deployment and a broken one.

// CheckpointLister reads the checkpoints written as a session executes.
//
// Note the shape: checkpoints are keyed by RUN. The port takes a session id
// because that is what an operator has in hand, and the implementation resolves
// the session's runs — but every row carries its run id, because a step index
// only means anything relative to the plan of the run it was taken against.
type CheckpointLister interface {
	CheckpointsForSession(sessionID string) ([]domain.CheckpointMeta, error)
	// ResumableAt reports whether a resume from this checkpoint is still valid.
	// Separated from the listing because it is a question about the CURRENT plan,
	// not a property recorded when the checkpoint was written.
	ResumableAt(runID string, stepIndex int) bool
}

// MCPServerLister enumerates the configured external MCP servers (ADR-0043) with
// live connection state.
type MCPServerLister interface {
	MCPServers() []MCPServerInfo
	// MCPConfigured distinguishes "no servers configured" from "MCP is not set up
	// on this build at all".
	MCPConfigured() bool
}

// MCPServerInfo is one MCP server as the operator plane reports it. Declared here
// rather than reusing the config type so the plane carries no config-schema
// coupling — and, more importantly, so no auth material can travel by accident:
// MCP auth is referenced by env-var NAME in config, and there is no field for it
// here at all.
type MCPServerInfo struct {
	Name      string
	Transport string
	Command   string
	URL       string
	Connected bool
	LastError string
	ToolCount int

	// The DECLARED half (contract 0097), so a console edit round-trips.
	// TokenConfigured/TokenSource are the only credential facts — the note above
	// about auth material still holds: no field here can carry a token.
	Endpoint           string
	Args               []string
	AuthType           string
	AuthHeader         string
	TokenConfigured    bool
	TokenSource        string
	ClassificationTags []string
}

// EmbeddingReporter reports the embedding model, its dimensions and the stored
// vector count.
type EmbeddingReporter interface {
	EmbeddingConfig() (provider, model, endpoint string, dimensions int)
	// VectorCount returns the number of stored embeddings, or -1 when it cannot
	// be counted. -1 is NOT zero: an operator reading "0 vectors" concludes the
	// corpus is empty, which is a different and much more alarming fact.
	VectorCount(ctx context.Context) int64
}

// InputClassifier is the ADR-0031 router, exposed as a decision WITHOUT the act.
type InputClassifier interface {
	Classify(ctx context.Context, text, surface string) (ClassifiedInput, error)
}

// ClassifiedInput is the router's decision. Classification is one of the five
// domain.DecisionType values — chat, plan, ingest, watch, clarification — and the
// plane passes it through verbatim rather than mapping it into a smaller set.
type ClassifiedInput struct {
	Classification string
	Why            string
	Confidence     float64
	Question       string
	Options        []string
}

// SetWave1Reads wires the contract-0072 read sources. Any may be nil.
func (s *Service) SetWave1Reads(
	checkpoints CheckpointLister,
	mcp MCPServerLister,
	embedding EmbeddingReporter,
	classifier InputClassifier,
) {
	s.checkpoints = checkpoints
	s.mcpServers = mcp
	s.embedding = embedding
	s.classifier = classifier
}

// ListSessionCheckpoints returns a session's checkpoints, grouped by run.
func (s *Service) ListSessionCheckpoints(_ context.Context, req *pb.ListSessionCheckpointsOpRequest) (*pb.ListSessionCheckpointsOpResponse, error) {
	if s.checkpoints == nil {
		return nil, status.Error(codes.Unimplemented, "no checkpoint store is wired on this kernel")
	}
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	metas, err := s.checkpoints.CheckpointsForSession(req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list checkpoints: %v", err)
	}

	// Ascending by (run, step) so a console can group without re-sorting, and so
	// paging by after_seq is stable.
	sort.SliceStable(metas, func(i, j int) bool {
		if metas[i].RunID != metas[j].RunID {
			return metas[i].RunID < metas[j].RunID
		}
		return metas[i].StepIndex < metas[j].StepIndex
	})

	out := make([]*pb.SessionCheckpointOp, 0, len(metas))
	for i, m := range metas {
		if int64(i) < req.GetAfterSeq() {
			continue
		}
		out = append(out, &pb.SessionCheckpointOp{
			SessionId:       string(m.SessionID),
			RunId:           string(m.RunID),
			PlanId:          m.PlanID,
			StepIndex:       int32(m.StepIndex),
			WrittenAtUnixMs: m.Timestamp.UnixMilli(),
			Resumable:       s.checkpoints.ResumableAt(string(m.RunID), m.StepIndex),
		})
	}
	return &pb.ListSessionCheckpointsOpResponse{Checkpoints: out}, nil
}

// ListMCPServers enumerates external MCP servers with live connection state.
func (s *Service) ListMCPServers(_ context.Context, _ *pb.ListMCPServersOpRequest) (*pb.ListMCPServersOpResponse, error) {
	if s.mcpServers == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report MCP servers")
	}
	servers := s.mcpServers.MCPServers()
	out := make([]*pb.MCPServerOp, 0, len(servers))
	for _, m := range servers {
		out = append(out, &pb.MCPServerOp{
			Name:               m.Name,
			Transport:          m.Transport,
			Command:            m.Command,
			Url:                m.URL,
			Connected:          m.Connected,
			LastError:          m.LastError,
			ToolCount:          int32(m.ToolCount),
			Endpoint:           m.Endpoint,
			Args:               m.Args,
			AuthType:           m.AuthType,
			AuthHeader:         m.AuthHeader,
			TokenConfigured:    m.TokenConfigured,
			TokenSource:        m.TokenSource,
			ClassificationTags: m.ClassificationTags,
		})
	}
	return &pb.ListMCPServersOpResponse{
		Servers:    out,
		Configured: s.mcpServers.MCPConfigured(),
	}, nil
}

// GetEmbeddingConfig reports the embedding model, dimensions and vector count.
func (s *Service) GetEmbeddingConfig(ctx context.Context, _ *pb.GetEmbeddingConfigOpRequest) (*pb.EmbeddingConfigOp, error) {
	if s.embedding == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report its embedding configuration")
	}
	provider, model, endpoint, dims := s.embedding.EmbeddingConfig()
	return &pb.EmbeddingConfigOp{
		Provider:    provider,
		Model:       model,
		Endpoint:    endpoint,
		Dimensions:  int32(dims),
		VectorCount: s.embedding.VectorCount(ctx),
	}, nil
}

// ClassifyInput returns the router's decision without acting on it.
func (s *Service) ClassifyInput(ctx context.Context, req *pb.ClassifyInputOpRequest) (*pb.ClassifiedInputOp, error) {
	if s.classifier == nil {
		return nil, status.Error(codes.Unimplemented, "no input router is wired on this kernel")
	}
	if req.GetText() == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	d, err := s.classifier.Classify(ctx, req.GetText(), req.GetSurface())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "classify: %v", err)
	}
	return &pb.ClassifiedInputOp{
		Classification:        d.Classification,
		Why:                   d.Why,
		Confidence:            d.Confidence,
		ClarificationQuestion: d.Question,
		ClarificationOptions:  d.Options,
	}, nil
}

// Wave1Capabilities returns the contract-0072 capability strings this build can
// actually serve.
//
// Advertised conditionally, per source, because the console gates surfaces on
// them (ADR-0082 D2) and will not probe. A capability advertised without a source
// behind it produces a screen that renders and then fails; a source without a
// capability produces a working surface nobody shows.
func (s *Service) Wave1Capabilities() []string {
	var caps []string
	if s.checkpoints != nil {
		// session-checkpoints: ListSessionCheckpoints serves run-keyed recovery
		// points, so the Checkpoints tab can fill.
		caps = append(caps, "session-checkpoints")
	}
	if s.mcpServers != nil {
		caps = append(caps, "mcp-registry")
	}
	if s.embedding != nil {
		caps = append(caps, "embedding-config")
	}
	if s.classifier != nil {
		// input-classification: ClassifyInput answers with the FIVE-value router
		// vocabulary. A client that only understands three must not claim this.
		caps = append(caps, "input-classification")
	}
	if s.generators != nil {
		caps = append(caps, "generator-registry")
	}
	return caps
}
