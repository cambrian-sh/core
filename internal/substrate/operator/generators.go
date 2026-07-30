package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// GeneratorRegistry is the read half of the generator surface (ADR-0042 config
// plus live breaker state), and the one real-call test.
//
// The write half — SaveGenerator / SetGeneratorKey / ClearGeneratorKey — is NOT
// here. It lands on the ADR-0101 config store, and building it against
// configs/providers.json in the meantime would both be thrown away and establish
// that the operator plane edits gitignored config files, which is the thing the
// store exists to stop.
type GeneratorRegistry interface {
	Generators() []GeneratorInfo
	RoleAssignments() []RoleAssignment
	// TestGenerator makes ONE real call against the generator's live endpoint.
	TestGenerator(ctx context.Context, id string) (GeneratorTestResult, error)
}

// GeneratorInfo is one configured generator plus its pulse.
//
// There is no field for the API key and there is deliberately no method that
// returns one. KeyLastFour and KeySource are what a console needs to answer "is
// the right key installed, and where is it coming from?" without any path by
// which the credential itself reaches a screen, a screen recording, or a log.
type GeneratorInfo struct {
	ID            string
	Provider      string
	Model         string
	Endpoint      string
	KeyConfigured bool
	KeyLastFour   string
	// KeySource is "env:<VAR>" or "store", matching the ADR-0101 precedence the
	// kernel actually applies when it resolves the credential.
	KeySource      string
	BreakerState   string
	RecentFailures int
	TimeoutMs      int64
	TokensInToday  int64
	TokensOutToday int64
	CallsToday     int64
	IsDefault      bool
	// The DECLARED half — what an operator authored, as opposed to the measured
	// pulse above. Reported so an edit round-trips: a console that cannot read
	// these back invents them on save, and writing an invented false over a true
	// NativeTools declaration silently disables tool-calling.
	Capabilities    []string
	NativeTools     bool
	DisableThinking bool
	// APIKeyEnv is a VARIABLE NAME, never a key.
	APIKeyEnv string
}

// RoleAssignment binds a system organ to the generator serving it.
type RoleAssignment struct {
	Role        string
	GeneratorID string
	// Resolved reports whether GeneratorID names a generator that exists. A role
	// pointing at a removed generator silently falls back to the default, and
	// nothing else in the system says so.
	Resolved bool
}

// GeneratorTestResult is one live probe.
type GeneratorTestResult struct {
	OK bool
	// ModelServed is the model string the ENDPOINT echoed back — the field this
	// whole RPC exists for. An endpoint answering happily with a different build
	// than the one requested is the most common misconfiguration in this space,
	// and a successful generation does not reveal it.
	ModelServed string
	// ModelRequested is what the probe ASKED for — the generator's configured
	// model. Both sides are needed or the comparison cannot be made here, and a
	// client left to guess the other side is what produced a mismatch verdict
	// beside two identical strings.
	ModelRequested string
	LatencyMs      int64
	Error          string
	Sample         string
}

// SetGeneratorRegistry wires the generator read surface. nil ⇒ Unimplemented.
func (s *Service) SetGeneratorRegistry(g GeneratorRegistry) { s.generators = g }

// ListGenerators reports configured generators with live breaker state.
func (s *Service) ListGenerators(_ context.Context, _ *pb.ListGeneratorsOpRequest) (*pb.ListGeneratorsOpResponse, error) {
	if s.generators == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report its generator registry")
	}
	gens := s.generators.Generators()
	out := make([]*pb.GeneratorOp, 0, len(gens))
	for _, g := range gens {
		out = append(out, &pb.GeneratorOp{
			Id:              g.ID,
			Provider:        g.Provider,
			Model:           g.Model,
			Endpoint:        g.Endpoint,
			KeyConfigured:   g.KeyConfigured,
			KeyLastFour:     g.KeyLastFour,
			KeySource:       g.KeySource,
			BreakerState:    g.BreakerState,
			RecentFailures:  int32(g.RecentFailures),
			TimeoutMs:       g.TimeoutMs,
			TokensInToday:   g.TokensInToday,
			TokensOutToday:  g.TokensOutToday,
			CallsToday:      g.CallsToday,
			IsDefault:       g.IsDefault,
			Capabilities:    g.Capabilities,
			NativeTools:     g.NativeTools,
			DisableThinking: g.DisableThinking,
		})
	}
	return &pb.ListGeneratorsOpResponse{Generators: out}, nil
}

// ListRoleAssignments reports which generator serves each system organ.
func (s *Service) ListRoleAssignments(_ context.Context, _ *pb.ListRoleAssignmentsOpRequest) (*pb.ListRoleAssignmentsOpResponse, error) {
	if s.generators == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report its generator registry")
	}
	roles := s.generators.RoleAssignments()
	out := make([]*pb.RoleAssignmentOp, 0, len(roles))
	for _, r := range roles {
		out = append(out, &pb.RoleAssignmentOp{
			Role:        r.Role,
			GeneratorId: r.GeneratorID,
			Resolved:    r.Resolved,
		})
	}
	return &pb.ListRoleAssignmentsOpResponse{Assignments: out}, nil
}

// TestGenerator makes one real call against a generator's endpoint.
//
// A failed probe is a SUCCESSFUL RPC carrying ok=false: the operator asked "does
// this work?" and got an answer. Returning a gRPC error would make a working
// diagnostic look like a broken console.
func (s *Service) TestGenerator(ctx context.Context, req *pb.TestGeneratorOpRequest) (*pb.GeneratorTestResultOp, error) {
	if s.generators == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report its generator registry")
	}
	if req.GetGeneratorId() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator_id is required")
	}
	res, err := s.generators.TestGenerator(ctx, req.GetGeneratorId())
	if err != nil {
		return &pb.GeneratorTestResultOp{Ok: false, Error: err.Error()}, nil
	}
	return &pb.GeneratorTestResultOp{
		Ok:             res.OK,
		ModelServed:    res.ModelServed,
		ModelRequested: res.ModelRequested,
		// Decided HERE, where both sides are known. An endpoint that echoes no
		// model at all cannot be said to match — that is unknown, not agreement.
		ModelMatches: res.ModelServed != "" && res.ModelServed == res.ModelRequested,
		LatencyMs:    res.LatencyMs,
		Error:        res.Error,
		Sample:       res.Sample,
		// The probe does a plain completion. Tool-calling is NOT exercised, and
		// saying so is the difference between "not tested" and "failed" — the
		// console was rendering the second.
		NativeToolsChecked: false,
		NativeToolsOk:      false,
	}, nil
}
