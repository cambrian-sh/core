package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// GeneratorWriter is the write half of the generator surface (contract 0083).
//
// The read half's comment named this as deliberately absent until the ADR-0101
// store existed, because the alternative — the operator plane editing
// configs/providers.json — is the practice that store was built to end. It
// exists now, so this lands on it.
type GeneratorWriter interface {
	// SaveGenerator creates or replaces one generator by id.
	//
	// The implementation must store the WHOLE effective list: the store layer
	// merges per key and a list is replaced wholesale, so persisting one entry
	// would silently delete every generator configured in a file beneath it.
	SaveGenerator(spec GeneratorSpec) (ConfigWriteOutcome, error)

	// RemoveGenerator drops one generator by id. Removing an id the kernel does
	// not have is an error rather than a no-op, because unlike a config key the
	// caller is naming a specific thing they believe exists, and silence there
	// hides a typo that leaves the real generator running.
	RemoveGenerator(id string) (ConfigWriteOutcome, error)
}

// GeneratorSpec is the DECLARED half of a generator — what an operator authors.
//
// Separate from GeneratorInfo, which is the read shape and carries measured
// pulse and credential facts. Accepting those back on a write would let a
// console send a stale breaker reading as if it were intent.
type GeneratorSpec struct {
	ID              string
	Provider        string
	Model           string
	Endpoint        string
	TimeoutMs       int64
	Capabilities    []string
	NativeTools     bool
	DisableThinking bool
	// APIKeyEnv names an environment variable, never a key.
	APIKeyEnv string
}

// SetGeneratorWriter wires the generator write path. nil ⇒ Unimplemented.
func (s *Service) SetGeneratorWriter(w GeneratorWriter) { s.generatorWriter = w }

// SaveGenerator creates or replaces a generator. Mutating: command_id + reason,
// audited, idempotent on command_id.
func (s *Service) SaveGenerator(ctx context.Context, req *pb.SaveGeneratorOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.generatorWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot persist generator changes")
	}
	g := req.GetGenerator()
	if g == nil || g.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator.id is required")
	}
	// Validated here rather than left to the store: a generator missing a model
	// or an endpoint is accepted by the config loader and fails at the first
	// call, which surfaces as a routing failure far from the edit that caused it.
	if g.GetModel() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator.model is required")
	}
	if g.GetProvider() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator.provider is required")
	}

	spec := GeneratorSpec{
		ID:              g.GetId(),
		Provider:        g.GetProvider(),
		Model:           g.GetModel(),
		Endpoint:        g.GetEndpoint(),
		TimeoutMs:       g.GetTimeoutMs(),
		Capabilities:    g.GetCapabilities(),
		NativeTools:     g.GetNativeTools(),
		DisableThinking: g.GetDisableThinking(),
		APIKeyEnv:       g.GetApiKeyEnv(),
	}

	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"save_generator", "generator", spec.ID,
		"generator "+spec.ID+" → "+spec.Provider+"/"+spec.Model,
		func() error {
			var applyErr error
			outcome, applyErr = s.generatorWriter.SaveGenerator(spec)
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}

// RemoveGenerator drops a generator from the stored list.
func (s *Service) RemoveGenerator(ctx context.Context, req *pb.RemoveGeneratorOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.generatorWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel cannot persist generator changes")
	}
	if req.GetGeneratorId() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator_id is required")
	}

	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"remove_generator", "generator", req.GetGeneratorId(),
		"generator "+req.GetGeneratorId()+" removed",
		func() error {
			var applyErr error
			outcome, applyErr = s.generatorWriter.RemoveGenerator(req.GetGeneratorId())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}
