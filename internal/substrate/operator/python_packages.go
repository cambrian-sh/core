package operator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/pypackages"
)

// PythonPackageAdmin is the sandbox Python lifecycle the operator plane drives
// (contract 0119, ENG-2012; ADR-0157 D7.10/D7.12 stage P5; pypackages.Admin).
type PythonPackageAdmin interface {
	List(ctx context.Context, target string) (pypackages.Listing, error)
	Remove(ctx context.Context, target, name, by, reason string) (string, error)
	Retry(ctx context.Context, target string) (string, error)
	Prune(ctx context.Context) pyenv.GCReport
}

func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func gcReportOp(r pyenv.GCReport) *pb.PythonGcReportOp {
	op := &pb.PythonGcReportOp{FreedBytes: r.FreedBytes, Errors: r.Errors, AtUnixMs: unixMs(r.At), Kept: int32(len(r.Kept))}
	for _, it := range r.Removed {
		op.Removed = append(op.Removed, &pb.PythonGcItemOp{Kind: it.Kind, Path: it.Path, Agent: it.Agent, Bytes: it.Bytes, Reason: it.Reason})
	}
	return op
}

// ListPythonPackages lists package records with their audit trail, the env's
// generations with sizes, and the last GC run (contract 0119). Operator-only
// (operatorOnlyReads): it shows the kernel's install layout.
func (s *Service) ListPythonPackages(ctx context.Context, req *pb.ListPythonPackagesOpRequest) (*pb.ListPythonPackagesOpResponse, error) {
	if s.pythonPackages == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no managed sandbox Python package service")
	}
	l, err := s.pythonPackages.List(ctx, req.GetTarget())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "python packages: %v", err)
	}
	resp := &pb.ListPythonPackagesOpResponse{
		Root: l.Inventory.Root, TotalBytes: l.Inventory.TotalBytes, CacheBytes: l.Inventory.CacheBytes,
		DownloadsBytes: l.Inventory.DownloadsBytes, LastGc: gcReportOp(l.LastGC),
	}
	for _, v := range l.Packages {
		r := v.Record
		op := &pb.PythonPackageOp{
			Id: r.ID, Target: r.Target, Name: r.Name, Version: r.Version, Extras: r.Extras, State: r.State,
			LockSha256: r.LockSHA256, BaseGeneration: r.BaseGeneration, ApprovalId: r.ApprovalID,
			RequestedBy: r.RequestedBy, RequestedAtUnixMs: unixMs(r.RequestedAt), DecidedBy: r.DecidedBy,
			DecidedAtUnixMs: unixMs(r.DecidedAt), Detail: r.Detail, Declared: v.Declared, Runtime: v.Runtime,
		}
		for _, e := range r.Lock.Entries {
			op.Distributions = append(op.Distributions, e.Name+"=="+e.Version)
		}
		for _, e := range v.Events {
			op.Events = append(op.Events, &pb.PythonPackageEventOp{Seq: e.Seq, Action: e.Action, Actor: e.Actor,
				Detail: e.Detail, Generation: e.Generation, AtUnixMs: unixMs(e.At)})
		}
		resp.Packages = append(resp.Packages, op)
	}
	for _, b := range l.Inventory.Bases {
		resp.Bases = append(resp.Bases, &pb.PythonBaseGenerationOp{Dir: b.Dir, Current: b.Current, Healthy: b.Healthy,
			BuiltAtUnixMs: unixMs(b.BuiltAt), Source: b.Source, Bytes: b.Bytes})
	}
	for _, g := range l.Inventory.Layers {
		op := &pb.PythonLayerGenerationOp{Agent: g.Agent, Dir: g.Dir, Base: g.Base, Healthy: g.Healthy,
			BuiltAtUnixMs: unixMs(g.BuiltAt), Bytes: g.Bytes, Current: samePath(l.Layers[g.Agent], g.Path)}
		for _, e := range g.Entries {
			op.Distributions = append(op.Distributions, e.Name+"=="+e.Version)
		}
		resp.Layers = append(resp.Layers, op)
	}
	return resp, nil
}

// RemovePythonPackage takes a runtime-requested package out of an agent's
// layer (contract 0119; D7.10.3): a new generation without it, the removal
// audited on the package record. A declared package is refused with the
// profile named. Mutating: command_id + reason, audited.
func (s *Service) RemovePythonPackage(ctx context.Context, req *pb.RemovePythonPackageOpRequest) (*pb.PythonPackageCommandAck, error) {
	if s.pythonPackages == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no managed sandbox Python package service")
	}
	if req.GetTarget() == "" || req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "target and name are required")
	}
	actor, _, _ := PrincipalFromContext(ctx)
	gen := ""
	ack, err := s.runMutationBefore(ctx, req.GetCommandId(), req.GetReason(), "remove_python_package", "agent", req.GetTarget(),
		"removed "+req.GetName(), func() (string, error) {
			g, err := s.pythonPackages.Remove(ctx, req.GetTarget(), req.GetName(), actor, req.GetReason())
			if err != nil {
				return "", packageErr(err)
			}
			gen = g
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	return &pb.PythonPackageCommandAck{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Generation: gen}, nil
}

// RetryPythonPackageInstall re-runs an agent's package check now (contract
// 0119; the D7.11 failure table's "operator can retry"). Mutating, audited.
func (s *Service) RetryPythonPackageInstall(ctx context.Context, req *pb.RetryPythonPackageInstallOpRequest) (*pb.PythonPackageCommandAck, error) {
	if s.pythonPackages == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no managed sandbox Python package service")
	}
	if req.GetTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "target is required")
	}
	gen := ""
	ack, err := s.runMutationBefore(ctx, req.GetCommandId(), req.GetReason(), "retry_python_package_install", "agent", req.GetTarget(),
		"", func() (string, error) {
			g, err := s.pythonPackages.Retry(ctx, req.GetTarget())
			if err != nil {
				return "", packageErr(err)
			}
			gen = g
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	return &pb.PythonPackageCommandAck{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped(), Generation: gen}, nil
}

// PrunePythonEnv runs one GC pass over the env root now (contract 0119;
// D7.12). Mutating, audited; the report says what was removed and freed.
func (s *Service) PrunePythonEnv(ctx context.Context, req *pb.PrunePythonEnvOpRequest) (*pb.PrunePythonEnvOpResponse, error) {
	if s.pythonPackages == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no managed sandbox Python package service")
	}
	var rep pyenv.GCReport
	ack, err := s.runMutationBefore(ctx, req.GetCommandId(), req.GetReason(), "prune_python_env", "python_env", "", "",
		func() (string, error) {
			rep = s.pythonPackages.Prune(ctx)
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	resp := &pb.PrunePythonEnvOpResponse{CommandId: ack.GetCommandId(), Deduped: ack.GetDeduped()}
	if !ack.GetDeduped() {
		resp.Report = gcReportOp(rep)
	}
	return resp, nil
}

// packageErr maps the package service's refusals onto gRPC codes.
func packageErr(err error) error {
	switch {
	case errors.Is(err, pypackages.ErrDeclaredPackage):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, pypackages.ErrNoSuchPackage):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, pypackages.ErrNotApproved), errors.Is(err, pypackages.ErrDeclaration):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Unavailable, "%v", err)
	}
}

// samePath compares two generation paths ("" never matches).
func samePath(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
