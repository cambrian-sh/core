package operator

import (
	"context"
	"sort"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pipeline authoring reads (contract 0089, ADR-0114 D35).
//
// Two RPCs, both side-effect free: read one authored revision, and compile a
// graph document without storing it. The canvas needs exactly these two and
// nothing more to render and to validate on change.
//
// Neither can write. That is the point rather than an omission — an editor
// holding an RPC that could publish is one bug away from publishing, and the
// cheapest way to guarantee it cannot is to not give it the power.

// SetPipelineAuthor wires the premium authoring read surface. nil (OSS) ⇒ both
// RPCs REFUSE by name.
//
// Refusing rather than answering empty, for the same reason as the dry run: an
// empty graph is a true-looking answer to "show me this pipeline" on a build
// that simply cannot, and a canvas would draw the empty graph rather than
// explain itself.
func (s *Service) SetPipelineAuthor(a domain.PipelineAuthor) { s.pipelineAuthor = a }

// GetPipeline returns one authored revision, plus what the compiler says about
// it right now.
func (s *Service) GetPipeline(ctx context.Context, req *pb.GetPipelineOpRequest) (*pb.GetPipelineOpResponse, error) {
	if s.pipelineAuthor == nil {
		return &pb.GetPipelineOpResponse{
			Refused: "this build has no pipeline runtime, so there is no graph to read",
		}, nil
	}
	if req.GetPipelineId() == "" {
		return &pb.GetPipelineOpResponse{Refused: "a pipeline id is required"}, nil
	}
	got, err := s.pipelineAuthor.GetPipeline(ctx, req.GetPipelineId(), int(req.GetRevision()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get pipeline: %v", err)
	}
	if got.Refused != "" {
		return &pb.GetPipelineOpResponse{Refused: got.Refused}, nil
	}
	p := got.Summary
	return &pb.GetPipelineOpResponse{
		Summary: &pb.PipelineOp{
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
		},
		GraphJson: got.GraphJSON,
		Refusals:  refusalsToProto(got.Refusals),
		Reads:     readsToProto(got.Reads),
	}, nil
}

// ValidatePipeline compiles a graph document without storing it.
func (s *Service) ValidatePipeline(ctx context.Context, req *pb.ValidatePipelineOpRequest) (*pb.ValidatePipelineOpResponse, error) {
	if s.pipelineAuthor == nil {
		return &pb.ValidatePipelineOpResponse{
			Refused: "this build has no pipeline compiler, so it cannot check a graph",
		}, nil
	}
	got, err := s.pipelineAuthor.ValidatePipeline(ctx, req.GetGraphJson())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "validate pipeline: %v", err)
	}
	return &pb.ValidatePipelineOpResponse{
		Refused:           got.Refused,
		Refusals:          refusalsToProto(got.Refusals),
		NodeCount:         int32(got.NodeCount),
		EffectNodeCount:   int32(got.EffectNodeCount),
		PlanChecksum:      got.PlanChecksum,
		SemanticsChecksum: got.SemanticsChecksum,
	}, nil
}

func readsToProto(in map[string][]string) []*pb.NodeReadsOp {
	out := make([]*pb.NodeReadsOp, 0, len(in))
	for node, paths := range in {
		out = append(out, &pb.NodeReadsOp{Node: node, Paths: paths})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func refusalsToProto(in []domain.PipelineRefusal) []*pb.RefusalOp {
	out := make([]*pb.RefusalOp, 0, len(in))
	for _, r := range in {
		out = append(out, &pb.RefusalOp{
			Constraint: r.Constraint,
			Node:       r.Node,
			Port:       r.Port,
			Message:    r.Message,
		})
	}
	return out
}

// ── Editing (contract 0090) ─────────────────────────────────────────────────

// SetPipelineWriter wires the premium draft-save surface. nil (OSS) ⇒
// SavePipeline REFUSES by name.
//
// A separate setter from SetPipelineAuthor, matching the separate port. The read
// surface's guarantee is that it cannot write, and a deployment can legitimately
// have one without the other — a console that reads pipelines on a build with no
// authoring is useful, and one that offers Save when nothing can store is not.
func (s *Service) SetPipelineWriter(w domain.PipelineWriter) { s.pipelineWriter = w }

// SavePipeline stores an edited graph as a new draft revision.
func (s *Service) SavePipeline(ctx context.Context, req *pb.SavePipelineOpRequest) (*pb.SavePipelineOpResponse, error) {
	if s.pipelineWriter == nil {
		return &pb.SavePipelineOpResponse{
			Refused: "this build cannot author pipelines, so there is nowhere to save one",
		}, nil
	}
	if req.GetGraphJson() == "" {
		return &pb.SavePipelineOpResponse{Refused: "there is no graph to save"}, nil
	}
	got, err := s.pipelineWriter.SavePipeline(ctx, req.GetGraphJson())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "save pipeline: %v", err)
	}
	return &pb.SavePipelineOpResponse{
		Refused:    got.Refused,
		PipelineId: got.PipelineID,
		Revision:   int32(got.Revision),
		Refusals:   refusalsToProto(got.Refusals),
	}, nil
}
