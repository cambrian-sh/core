package executer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// The DAG executor's half of the run ledger (ADR-0145 D1): the step-output choke
// point, and the read that hands dependants their upstream results FROM the ledger.
//
// When DAGExecutor.Ledger is set the server builds the executor with no
// ContentStore, so step results are no longer plan-scoped content-store scratch:
// step_N_cid holds the step's ledger id, the dependency manifest's cid is that id
// (resolvable by the worker through GetContextNode / ledger_get), and the content a
// dependant receives is read back from the ledger, not taken from the masterContext
// string — masterContext's step_N_result stays only as the in-memory view the plan
// report, the step cache key and fan-out extraction read.
//
// Legacy path (Ledger nil, execution.ledger.enabled off): step results go to the
// content store as before and are evicted at plan end. It is kept until the ledger
// has passed the ADR-0145 phase-1 gate and is on by default; then the content-store
// branch of mergeStepResult and the plan-end GC of step results are deleted.

// recordStepOutput records one step's output as a step_output entry and returns
// its id ("" when nothing was recorded). Called by the coordinator OUTSIDE the
// context lock. Every step that returned a response is recorded — an empty output
// included, so a dependant reads "empty", not "missing".
//
// inputs are the ledger ids of the step's upstream outputs (stepInputRefs): the
// recorder carries their classification forward, together with the step's own
// tool results', so the output is never easier to read than what it was built
// from (T-20261108-ledger-step-outputs-do-not-inherit-the).
func (d *DAGExecutor) recordStepOutput(ctx context.Context, plan *domain.ExecutionPlan, r stepResult, inputs []string) string {
	if d.Ledger == nil || r.resp == nil {
		return ""
	}
	var body []byte
	if r.resp.Payload != nil {
		body = r.resp.Payload.Data
	}
	writer := domain.AgentPrincipal(r.resp.FromAgent)
	if r.index >= 0 && r.index < len(plan.Steps) && plan.Steps[r.index].IsThought {
		writer = domain.SystemPrincipal // an in-kernel model call, no agent
	}
	p := d.ledgerProvenance(ctx)
	p.StepIndex = r.index
	ct := "text/plain"
	if len(body) > 0 && json.Valid(body) {
		ct = "application/json"
	}
	ent, err := d.Ledger.Record(ctx, domain.LedgerRecord{
		Kind:        domain.LedgerStepOutput,
		Provenance:  p,
		Writer:      writer,
		DerivedFrom: inputs,
		ContentType: ct,
		Body:        body,
	})
	if err != nil {
		slog.Warn("ADR-0145: step output not recorded to the ledger; its dependants will read it as unavailable",
			"step", r.index, "err", err)
		return ""
	}
	return ent.ID
}

// ledgerProvenance is the run's provenance (set by the server on the plan ctx),
// with this executor's run id when the ctx carries none, under the current plan
// generation's ledger run id (domain.LedgerRunID: after a replan the new plan's
// step indices start again, so its entries must not share the old plan's run id;
// T-20261108-ledger-step-indices-collide-across-replans-in). The tool executor
// records a step's tool results under the same id (LeaseBinding.PlanGeneration).
func (d *DAGExecutor) ledgerProvenance(ctx context.Context) domain.LedgerProvenance {
	p := domain.LedgerProvenanceFromContext(ctx)
	if p.RunID == "" {
		p.RunID = string(d.CurrentRunID)
	}
	p.RunID = domain.LedgerRunID(p.RunID, d.planGeneration())
	return p
}

// planGeneration is the executor's plan generation: 0 for the plan it started
// with, +1 for every replan, hot swap or injection (applyReplannedPlan). A
// fan-out expansion keeps the generation: it appends children at fresh indices.
func (d *DAGExecutor) planGeneration() int {
	d.planMu.Lock()
	defer d.planMu.Unlock()
	return d.generation
}

// ledgerResolver returns the content reader buildStepDependenciesFrom uses when the
// ledger is on, nil otherwise. It reads under LedgerReadScope — the kernel acting
// for the run's owner — through the ENFORCED reader.
func (d *DAGExecutor) ledgerResolver(ctx context.Context) dependencyResolver {
	if d.Ledger == nil || d.LedgerReader == nil {
		return nil
	}
	scope := d.LedgerReadScope
	if scope == nil {
		scope = &domain.TagPredicate{Bypass: true}
	}
	rctx := domain.WithScope(ctx, scope)
	return func(ref string) (string, error) {
		if !domain.IsLedgerID(ref) {
			return "", fmt.Errorf("no ledger entry for this step (the output was not recorded)")
		}
		e, err := d.LedgerReader.Get(rctx, ref)
		if err != nil {
			return "", err
		}
		if e == nil {
			return "", fmt.Errorf("ledger entry %s is not readable", ref)
		}
		return string(e.Body), nil
	}
}

// stepInputRefs is the ledger ids of step i's upstream outputs as recorded in
// master (step_N_cid of each dependency). Caller holds the context lock.
func stepInputRefs(plan *domain.ExecutionPlan, i int, master map[string]string) []string {
	if plan == nil || i < 0 || i >= len(plan.Steps) {
		return nil
	}
	var out []string
	for _, dep := range plan.Steps[i].DependsOn {
		if id := master[fmt.Sprintf("step_%d_cid", dep)]; domain.IsLedgerID(id) {
			out = append(out, id)
		}
	}
	return out
}
