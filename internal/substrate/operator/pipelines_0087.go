package operator

import (
	"context"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reactive pipelines read surface (contract 0087, ADR-0114 D33/D34).
//
// Listed separately from watches because they are different things, and
// collapsing them would force one shape to lie about the other: a watch has one
// condition and a list of arms but no revisions and no nodes; a pipeline is a
// versioned graph with typed ports and a durable execution store, and has no
// single condition to report.
//
// The console needs both — a deployment can run watches and pipelines side by
// side (D33) — so the answer is two surfaces, not one widened until it fits
// neither.

// SetPipelineLister wires the premium pipeline read surface. nil (OSS) ⇒
// ListPipelines returns an EMPTY LIST rather than Unimplemented.
//
// Empty rather than an error on purpose, and unlike the watch RPCs: "this build
// authors no pipelines" is a true answer to the question asked, and a console
// that has to special-case an error to render an empty panel will eventually
// render the error instead.
func (s *Service) SetPipelineLister(l domain.PipelineLister) { s.pipelines = l }

// ListPipelines returns authored pipeline revisions.
func (s *Service) ListPipelines(ctx context.Context, req *pb.ListPipelinesOpRequest) (*pb.ListPipelinesOpResponse, error) {
	if s.pipelines == nil {
		return &pb.ListPipelinesOpResponse{}, nil
	}
	found, err := s.pipelines.ListPipelines(ctx, req.GetArmedOnly(), req.GetIngressId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list pipelines: %v", err)
	}
	out := make([]*pb.PipelineOp, 0, len(found))
	for _, p := range found {
		out = append(out, &pb.PipelineOp{
			PipelineId:        p.PipelineID,
			Revision:          int32(p.Revision),
			Name:              p.Name,
			State:             p.State,
			TriggerType:       p.TriggerType,
			TriggerRef:        p.TriggerRef,
			NodeCount:         int32(p.NodeCount),
			EdgeCount:         int32(p.EdgeCount),
			EffectNodeCount:   int32(p.EffectNodeCount),
			MappingRevision:   int32(p.MappingRevision),
			PlanChecksum:      p.PlanChecksum,
			SemanticsChecksum: p.SemanticsChecksum,
			Generated:         p.Generated,
			DryRun:            p.DryRun,
			Approved:          p.Approved,
			EntryLive:         p.EntryLive,
		})
	}
	return &pb.ListPipelinesOpResponse{Pipelines: out}, nil
}

// ── Dry run (contract 0088) ─────────────────────────────────────────────────

// SetPipelineDryRunner wires the premium shadow-run surface. nil (OSS) ⇒
// DryRunPipeline REFUSES by name.
//
// Deliberately unlike SetPipelineLister above. An empty list is a true answer to
// "which pipelines exist"; an empty report is NOT a true answer to "what would
// this do", because it is indistinguishable from a pipeline that would do
// nothing. Refusing by name is the only honest response a build without a
// pipeline runtime can give.
func (s *Service) SetPipelineDryRunner(r domain.PipelineDryRunner) { s.pipelineDryRun = r }

// DryRunPipeline replays captured deliveries with every effect shadowed.
func (s *Service) DryRunPipeline(ctx context.Context, req *pb.DryRunPipelineOpRequest) (*pb.DryRunPipelineOpResponse, error) {
	if s.pipelineDryRun == nil {
		return &pb.DryRunPipelineOpResponse{
			Refused: "this build has no pipeline runtime, so it cannot dry-run one",
		}, nil
	}
	if req.GetPipelineId() == "" {
		return &pb.DryRunPipelineOpResponse{Refused: "a dry run needs a pipeline id"}, nil
	}
	got, err := s.pipelineDryRun.DryRunPipeline(ctx, req.GetPipelineId(), int(req.GetRevision()), int(req.GetSampleLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dry run pipeline: %v", err)
	}
	// A refusal is carried in the response, not as a gRPC error: it is a named
	// constraint the console renders verbatim, and an error status would reduce
	// it to a transport failure the UI has to guess at.
	if got.Refused != "" {
		return &pb.DryRunPipelineOpResponse{Refused: got.Refused}, nil
	}

	effects := make([]*pb.ShadowEffectOp, 0, len(got.Effects))
	for _, e := range got.Effects {
		effects = append(effects, &pb.ShadowEffectOp{
			Node: e.Node, Kind: e.Kind, EffectKey: e.EffectKey,
			ItemKey: e.ItemKey, Summary: e.Summary,
		})
	}
	dupes := make([]*pb.DuplicateEffectKeyOp, 0, len(got.Duplicates))
	for _, d := range got.Duplicates {
		dupes = append(dupes, &pb.DuplicateEffectKeyOp{
			EffectKey: d.EffectKey, Count: int32(d.Count), Nodes: d.Nodes,
		})
	}
	terms := make([]*pb.PipelineTerminationOp, 0, len(got.Terminations))
	for _, t := range got.Terminations {
		terms = append(terms, &pb.PipelineTerminationOp{
			Node: t.Node, Port: t.Port, Reason: t.Reason, Count: int32(t.Count),
		})
	}
	fails := make([]*pb.PipelineFailureOp, 0, len(got.Failures))
	for _, f := range got.Failures {
		fails = append(fails, &pb.PipelineFailureOp{Node: f.Node, ItemKey: f.ItemKey, Err: f.Err})
	}
	return &pb.DryRunPipelineOpResponse{
		RunId:        got.RunID,
		Samples:      int32(got.Samples),
		Effects:      effects,
		Duplicates:   dupes,
		Terminations: terms,
		Failures:     fails,
		ElapsedMs:    got.ElapsedMs,
	}, nil
}
