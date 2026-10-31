package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// orchestratorExecutor is the one method of the substrate Server the plan executor
// needs. Narrowed so the adapter can be tested without assembling a kernel.
type orchestratorExecutor interface {
	Execute(ctx context.Context, in *pb.Handoff) (*pb.Handoff, error)
}

// errPlanExecutorUnbound is returned when a plugin runs a plan before the server it
// delegates to exists (or in a kernel built without one).
var errPlanExecutorUnbound = errors.New("plan executor: kernel server not bound yet")

// kernelPlanExecutor is KernelServices.PlanExecutor (domain.PlanExecutor): it runs a
// plugin-generated plan through the kernel's REAL DAG executor.
//
// It does that by submitting the plan on the Orchestrator.Execute path as an
// already-built plan (subnetwork.AuthoredPlanMetadataKey, contract 0074), rather than by
// constructing an executer.DAGExecutor here. That path is the only place the executor is
// assembled with its step function (auction dispatch, self-healing, fallback), per-step
// ADR-0018 budget leases, session + run persistence, checkpoints and the operator control
// hub; building a second one for plugins would be a parallel execution pathway that
// drifts from it (AGENTS.md §4.3). The authored-plan branch skips the router and the
// planner — the plugin already planned — and still re-validates the DAG
// (TopologicalSort) as the last gate before execution.
//
// Late-bound for the same reason as kernelMemoryIngestor: the KernelServices bundle is
// built, and plugins Built, before the server is constructed. A plugin holds this
// pointer from Build and calls it at fire time, by which point bind has run.
type kernelPlanExecutor struct {
	srv atomic.Pointer[orchestratorExecutor]
}

// bind installs the server. Called once, right after the server is constructed.
func (p *kernelPlanExecutor) bind(s orchestratorExecutor) {
	if s == nil {
		p.srv.Store(nil)
		return
	}
	p.srv.Store(&s)
}

// Execute implements domain.PlanExecutor.
func (p *kernelPlanExecutor) Execute(ctx context.Context, plan *domain.ExecutionPlan, h *domain.Handoff) (*domain.Handoff, error) {
	if plan == nil {
		return nil, errors.New("plan executor: nil plan")
	}
	sp := p.srv.Load()
	if sp == nil || *sp == nil {
		return nil, errPlanExecutorUnbound
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("plan executor: marshal plan: %w", err)
	}

	md := map[string]string{}
	in := &pb.Handoff{FromAgent: "plugin"}
	if h != nil {
		for k, v := range h.Context {
			md[k] = v
		}
		in.Id = h.ID
		if h.FromAgent != "" {
			in.FromAgent = h.FromAgent
		}
		if h.Payload != nil {
			in.Payload = &pb.Object{Type: h.Payload.Type, Data: h.Payload.Data}
		}
	}
	// Set AFTER the caller's context, so a seeded key can neither replace the plan
	// being run nor turn this into a resume of some other run (Phase 3: a resume is
	// only ever an explicit operator request).
	md[subnetwork.AuthoredPlanMetadataKey] = string(planJSON)
	delete(md, subnetwork.ResumeMetadataKey)
	in.Metadata = md
	if in.Payload == nil {
		// The payload is the run's original_prompt and the session goal; the plan's
		// subject is the best description of the work when the caller gave none.
		in.Payload = &pb.Object{Data: []byte(plan.Subject)}
	}

	out, err := (*sp).Execute(ctx, in)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	res := &domain.Handoff{
		ID:        out.GetId(),
		FromAgent: out.GetFromAgent(),
		ToAgent:   out.GetToAgent(),
		Context:   out.GetMetadata(),
	}
	if o := out.GetPayload(); o != nil {
		res.Payload = &domain.Payload{Type: o.GetType(), Data: o.GetData()}
	}
	return res, nil
}

var _ domain.PlanExecutor = (*kernelPlanExecutor)(nil)
