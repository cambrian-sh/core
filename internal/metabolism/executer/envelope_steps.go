package executer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/requirements"
)

// The DAG executor's half of ADR-0145 D5/D6 (execution.plan.checklist_enabled;
// T-20261108-adr-0145-phase-2-requirement-checklist-with): a step that answers
// checklist items is told which (HandoffKeyAnswerItems) and is handed its bound
// inputs (HandoffKeyStepInputs); its output must be the fixed answer envelope,
// which is validated here against the run ledger — types, evidence from the
// step's own tool calls or inputs, figures from tool results or calculations, and
// the deep check — with ONE repair turn naming the violations; and what survives
// is applied to the request's Coverage, the only place item statuses change.
//
// Legacy path (Requirements nil): no item keys are stamped, outputs are prose, and
// nothing is validated. It is deleted with the flag once the checklist defaults on
// (T-20261108-delete-the-pre-checklist-prose-step-path-once).

// StepRequirements is the per-request state the executor needs for D5/D6. Built
// by the server for one Execute (and reused across its coverage stages).
type StepRequirements struct {
	Checklist *domain.RequirementChecklist
	Coverage  *requirements.Coverage
	// StepActions returns the kernel's records of the calls one step made: its
	// side-effecting calls (the plan action ledger, network/plan_actions.go) and its
	// refused reads, for the step's plan generation (step indices restart after a
	// replan). nil ⇒ none known.
	StepActions func(generation, step int) StepRecords

	mu sync.Mutex
	// evidence are the accepted evidence refs per (stage-local) step: a dependant
	// may cite what its inputs cited (D6: "from the step's own tool calls or inputs").
	evidence map[int][]string
	// values are the accepted values per step, for input bindings.
	values map[int][]domain.EnvelopeValue
	// repeated are the violations (item|rule|value) that a repair turn was told
	// about and that came back unchanged. Kept for the whole request (every
	// coverage stage): such a violation is never repaired again.
	repeated map[string]bool
	// earlier are the finished stages' results under their e<K> labels
	// (ArchiveStage): what a later stage's e<K> input resolves to.
	earlier map[string]archivedResult
}

// archivedResult is an earlier stage's step as an e<K> input resolves it.
type archivedResult struct {
	domain.EarlierResult
	values   []domain.EnvelopeValue
	evidence []string
}

// ArchiveStage keeps the finished stage's accepted values and evidence under the
// e<K> labels the server gave its completed steps (es: that stage's entries,
// whose Step indices are this stage's). Call it before ResetStage; a later
// stage's e<K> input then resolves to the step's ledger reference and its values
// (T-20261112-staged-planner-binds-earlier-stage-results-with).
func (r *StepRequirements) ArchiveStage(es []domain.EarlierResult) {
	if r == nil || len(es) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.earlier == nil {
		r.earlier = map[string]archivedResult{}
	}
	for _, e := range es {
		r.earlier[e.Label] = archivedResult{EarlierResult: e,
			values:   append([]domain.EnvelopeValue(nil), r.values[e.Step]...),
			evidence: append([]string(nil), r.evidence[e.Step]...)}
	}
}

// earlierInputEvidence are the ledger ids a step's e<K> inputs make citable: each
// earlier step's output and the entries its accepted values cited (D6: a step may
// cite its inputs and what they cited).
func (r *StepRequirements) earlierInputEvidence(st domain.Step) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, b := range st.Inputs {
		ref, ok := domain.ParseInputRef(b)
		if !ok || !ref.Earlier {
			continue
		}
		if a, ok := r.earlier[ref.Source()]; ok {
			out = appendUnique(out, a.Ref)
			for _, id := range a.evidence {
				out = appendUnique(out, id)
			}
		}
	}
	return out
}

func violationKey(v domain.EnvelopeViolation) string {
	return v.Item + "|" + v.Rule + "|" + v.Value
}

// unrepaired are the violations no earlier repair turn of this request already
// failed to fix (all of vs when r is nil).
func (r *StepRequirements) unrepaired(vs []domain.EnvelopeViolation) []domain.EnvelopeViolation {
	if r == nil || len(vs) == 0 {
		return vs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.EnvelopeViolation
	for _, v := range vs {
		if !r.repeated[violationKey(v)] {
			out = append(out, v)
		}
	}
	return out
}

// noteRepaired records the violations a repair turn was told about (asked) that
// are still there after it (after).
func (r *StepRequirements) noteRepaired(asked, after []domain.EnvelopeViolation) {
	if r == nil {
		return
	}
	still := map[string]bool{}
	for _, v := range after {
		still[violationKey(v)] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repeated == nil {
		r.repeated = map[string]bool{}
	}
	for _, v := range asked {
		if k := violationKey(v); still[k] {
			r.repeated[k] = true
		}
	}
}

// StepRecords are the kernel's records of one step's tool calls.
type StepRecords struct {
	// Actions are the side-effecting calls (done, failed or refused).
	Actions []domain.PlanAction
	// Denials are the refused read calls.
	Denials []domain.PlanAction
}

// NewStepRequirements starts a request's executor state.
func NewStepRequirements(c *domain.RequirementChecklist, cov *requirements.Coverage, actions func(generation, step int) StepRecords) *StepRequirements {
	return &StepRequirements{Checklist: c, Coverage: cov, StepActions: actions,
		evidence: map[int][]string{}, values: map[int][]domain.EnvelopeValue{}}
}

// ResetStage forgets the per-step evidence and values of the previous stage: a new
// stage is a new plan whose step indices start again at 0. Coverage persists.
func (r *StepRequirements) ResetStage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evidence = map[int][]string{}
	r.values = map[int][]domain.EnvelopeValue{}
}

// stepItems are the checklist items step answers (unknown ids skipped).
func (r *StepRequirements) stepItems(st domain.Step) []domain.RequirementItem {
	if r == nil || r.Checklist == nil {
		return nil
	}
	var out []domain.RequirementItem
	for _, id := range st.Answers {
		if it := r.Checklist.Item(id); it != nil {
			out = append(out, *it)
		}
	}
	return out
}

// stampRequirementKeys writes (or removes) the D5/D6 handoff keys on a step's
// snapshot. Kernel-authored: always overwritten, removed from every step that
// does not carry them.
func (d *DAGExecutor) stampRequirementKeys(plan *domain.ExecutionPlan, stepIndex int, snapshot map[string]string) {
	delete(snapshot, domain.HandoffKeyAnswerItems)
	delete(snapshot, domain.HandoffKeyStepInputs)
	delete(snapshot, domain.HandoffKeyEnvelopeViolations)
	delete(snapshot, domain.HandoffKeyRepairPriorCalls)
	if d.Requirements == nil {
		return
	}
	st := plan.Steps[stepIndex]
	if items := d.Requirements.stepItems(st); len(items) > 0 {
		if b, err := json.Marshal(items); err == nil {
			snapshot[domain.HandoffKeyAnswerItems] = string(b)
		}
	}
	if in := d.Requirements.resolveInputs(st, snapshot); len(in) > 0 {
		if b, err := json.Marshal(in); err == nil {
			snapshot[domain.HandoffKeyStepInputs] = string(b)
		}
	}
}

// resolveInputs resolves a step's input references (domain.InputRef): for s<N>,
// the bound step's ledger reference and, for s<N>.<name>, the accepted values step
// N returned under that name; for e<K>, the same of the earlier stage's step the
// label names (ArchiveStage). A reference that resolves to nothing is skipped (the
// D7 validator refuses it before the plan runs).
func (r *StepRequirements) resolveInputs(st domain.Step, snapshot map[string]string) []domain.StepInputValue {
	if len(st.Inputs) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.StepInputValue
	for _, b := range st.Inputs {
		ref, ok := domain.ParseInputRef(b)
		if !ok {
			continue
		}
		var iv domain.StepInputValue
		var values []domain.EnvelopeValue
		if ref.Earlier {
			a, ok := r.earlier[ref.Source()]
			if !ok {
				continue
			}
			iv = domain.StepInputValue{Binding: b, Step: a.Step, Stage: a.Stage, Ref: a.Ref}
			values = a.values
		} else {
			iv = domain.StepInputValue{Binding: b, Step: ref.N, Ref: snapshot[fmt.Sprintf("step_%d_cid", ref.N)]}
			values = r.values[ref.N]
		}
		for _, v := range values {
			if ref.Name == "" || v.Name == ref.Name {
				iv.Values = append(iv.Values, v)
			}
		}
		out = append(out, iv)
	}
	return out
}

// envelopeStep is the D6 check of one step's response. It returns the response
// to merge (the repaired one when the repair turn ran). A step that answers no
// item, a thought step, and every step on the legacy path pass through untouched.
func (d *DAGExecutor) envelopeStep(ctx context.Context, plan *domain.ExecutionPlan, stepIndex int,
	stepHandoff *domain.Handoff, stepFn StepFunc, resp *domain.Handoff, inputs []string) *domain.Handoff {
	if d.Requirements == nil || resp == nil || plan.Steps[stepIndex].IsThought {
		return resp
	}
	st := plan.Steps[stepIndex]
	items := d.Requirements.stepItems(st)
	if len(items) == 0 {
		return resp
	}
	// An e<K> input's entries are the step's inputs too (an earlier stage's output).
	inputs = append(append([]string(nil), inputs...), d.Requirements.earlierInputEvidence(st)...)
	results, violations, obs := d.checkEnvelope(ctx, stepIndex, items, resp, inputs)
	// A violation that already survived a repair turn in this request is not
	// repaired again (research note 2026-11-12 §5: a repeated error is
	// deterministic; asking the same way twice gets the same answer). Only the
	// violations not yet repaired are named in the repair turn; when there are
	// none, the turn is skipped and the items stay open with the reason.
	toRepair := d.Requirements.unrepaired(violations)
	if len(violations) > 0 && len(toRepair) == 0 {
		slog.Info("ADR-0145 D6: step envelope rejected with violations that survived an earlier repair turn; no repair turn",
			"step", stepIndex, "violations", violations)
	}
	if len(toRepair) > 0 {
		// The ONE repair turn (D6.2): the same step again, told exactly what was
		// wrong. Its tool calls land on the same run and step, so evidence it
		// gathers now counts.
		slog.Info("ADR-0145 D6: step envelope rejected; one repair turn", "step", stepIndex, "violations", len(toRepair))
		repair := cloneMap(stepHandoff.Context)
		if b, err := json.Marshal(toRepair); err == nil {
			repair[domain.HandoffKeyEnvelopeViolations] = string(b)
		}
		// ADR-0145 D14: the repair sees what the step already did (tool results by
		// ledger reference, writes with their outcome) and its lease is marked as a
		// repair turn, so the kernel refuses a write the step already executed.
		if prior := d.repairPriorCalls(ctx, stepIndex, obs); len(prior) > 0 {
			if b, err := json.Marshal(prior); err == nil {
				repair[domain.HandoffKeyRepairPriorCalls] = string(b)
			}
		}
		h := &domain.Handoff{Payload: stepHandoff.Payload, Context: repair, WorkingMemory: stepHandoff.WorkingMemory}
		if r2, err := stepFn(domain.WithRepairTurn(domain.WithPlanStep(ctx, st)), stepIndex, h); err == nil && r2 != nil {
			resp = r2
			results, violations, obs = d.checkEnvelope(ctx, stepIndex, items, resp, inputs)
			d.Requirements.noteRepaired(toRepair, violations)
		} else if err != nil {
			slog.Warn("ADR-0145 D6: repair turn failed; the step's items stay open", "step", stepIndex, "err", err)
		}
	}
	obs.Results = results
	obs.Violations = violations
	obs.Member = st.FanOutItem
	obs.SetMembers = fanOutDependencyMembers(plan, st)
	if d.Requirements.StepActions != nil {
		rec := d.Requirements.StepActions(d.planGeneration(), stepIndex)
		obs.Actions, obs.Denials = rec.Actions, rec.Denials
	}
	if d.Requirements.Coverage != nil {
		d.Requirements.Coverage.ObserveStep(obs)
	}
	r := d.Requirements
	r.mu.Lock()
	for _, res := range results {
		for _, v := range res.Values {
			r.values[stepIndex] = append(r.values[stepIndex], v)
			r.evidence[stepIndex] = appendUnique(r.evidence[stepIndex], v.EvidenceRef)
		}
	}
	r.mu.Unlock()
	if len(violations) > 0 {
		slog.Warn("ADR-0145 D6: step envelope still invalid after the repair turn; the affected items stay open",
			"step", stepIndex, "violations", violations)
	}
	return resp
}

// maxRepairPriorCalls bounds the prior-call list a repair turn is handed.
const maxRepairPriorCalls = 40

// repairPriorCalls is what the step did before its repair turn: its tool results
// in the ledger (by reference, with a preview) and its side-effecting calls from
// the action ledger with their outcome.
func (d *DAGExecutor) repairPriorCalls(ctx context.Context, stepIndex int, _ requirements.StepObservation) []domain.RepairPriorCall {
	var out []domain.RepairPriorCall
	if d.Requirements != nil && d.Requirements.StepActions != nil {
		for _, a := range d.Requirements.StepActions(d.planGeneration(), stepIndex).Actions {
			out = append(out, domain.RepairPriorCall{Tool: a.Tool, Write: true, Status: a.Status, IDs: a.IDs})
		}
	}
	if d.LedgerReader != nil {
		if p := d.ledgerProvenance(ctx); p.RunID != "" {
			scope := d.LedgerReadScope
			if scope == nil {
				scope = &domain.TagPredicate{Bypass: true}
			}
			rows, err := d.LedgerReader.Find(domain.WithScope(ctx, scope), domain.LedgerQuery{RunID: p.RunID, StepIndex: stepIndex, HasStep: true})
			if err == nil {
				for _, row := range rows {
					if row.Kind != domain.LedgerToolResult && row.Kind != domain.LedgerComputed {
						continue
					}
					out = append(out, domain.RepairPriorCall{Tool: row.ToolName, LedgerRef: row.ID, Preview: row.Preview})
				}
			}
		}
	}
	if len(out) > maxRepairPriorCalls {
		out = out[:maxRepairPriorCalls]
	}
	return out
}

// checkEnvelope parses and validates one response against the step's evidence.
func (d *DAGExecutor) checkEnvelope(ctx context.Context, stepIndex int, items []domain.RequirementItem,
	resp *domain.Handoff, inputs []string) ([]domain.ItemResult, []domain.EnvelopeViolation, requirements.StepObservation) {
	obs := requirements.StepObservation{Step: stepIndex, EvidenceKinds: map[string]domain.LedgerKind{}}
	ev := d.stepEvidence(ctx, stepIndex, inputs, &obs)
	var body []byte
	if resp.Payload != nil {
		body = resp.Payload.Data
	}
	env, ok := domain.ParseAnswerEnvelope(body)
	if !ok {
		env = nil
	}
	if env != nil {
		obs.AnswerText = env.AnswerText
		d.admitRequestEvidence(ctx, env, &ev, &obs)
	}
	results, vs := requirements.ValidateEnvelope(env, items, ev)
	return results, vs, obs
}

// stepEvidence is what step may cite: its own tool results and calculations in
// this run, its input entries (the upstream step outputs), the tool results and
// calculations of the steps it depends on (transitively), and the entries those
// inputs' envelopes cited. Read through the ENFORCED reader under the run's read
// scope, bodies included (the deep check needs them). obs gets the step's search
// counts and every entry's kind.
func (d *DAGExecutor) stepEvidence(ctx context.Context, stepIndex int, inputs []string, obs *requirements.StepObservation) requirements.Evidence {
	ev := requirements.Evidence{Entries: map[string]domain.LedgerEntry{}}
	if d.LedgerReader == nil {
		return ev
	}
	scope := d.LedgerReadScope
	if scope == nil {
		scope = &domain.TagPredicate{Bypass: true}
	}
	rctx := domain.WithScope(ctx, scope)
	p := d.ledgerProvenance(ctx)
	get := func(id string) {
		if _, have := ev.Entries[id]; have || !domain.IsLedgerID(id) {
			return
		}
		e, err := d.LedgerReader.Get(rctx, id)
		if err != nil || e == nil {
			return
		}
		ev.Entries[id] = *e
		obs.EvidenceKinds[id] = e.Kind
	}
	if p.RunID != "" {
		rows, err := d.LedgerReader.Find(rctx, domain.LedgerQuery{RunID: p.RunID, StepIndex: stepIndex, HasStep: true})
		if err != nil {
			slog.Warn("ADR-0145 D6: the step's ledger entries could not be listed", "step", stepIndex, "err", err)
		}
		for _, row := range rows {
			if row.Kind != domain.LedgerToolResult && row.Kind != domain.LedgerComputed && row.Kind != domain.LedgerArtifact {
				continue
			}
			get(row.ID)
			if row.Kind == domain.LedgerToolResult {
				obs.ToolResults++
				if e, ok := ev.Entries[row.ID]; ok && isErrorResult(e.Body) {
					obs.ToolErrors++
				}
			}
		}
	}
	for _, id := range inputs {
		get(id)
	}
	// The inputs' own evidence: the tool results and calculations of every step
	// this one depends on, transitively (the data its inputs were built from), so
	// a figure an upstream step fetched can be cited where it is used (o3x: the
	// overdue amounts listed in step 0 reach the total stated in step 1).
	if p.RunID != "" {
		for _, dep := range d.depsOf(stepIndex) {
			rows, err := d.LedgerReader.Find(rctx, domain.LedgerQuery{RunID: p.RunID, StepIndex: dep, HasStep: true})
			if err != nil {
				continue
			}
			for _, row := range rows {
				if row.Kind == domain.LedgerToolResult || row.Kind == domain.LedgerComputed || row.Kind == domain.LedgerArtifact {
					get(row.ID)
				}
			}
		}
	}
	if d.Requirements != nil {
		d.Requirements.mu.Lock()
		var cited []string
		for _, dep := range d.depsOf(stepIndex) {
			cited = append(cited, d.Requirements.evidence[dep]...)
		}
		d.Requirements.mu.Unlock()
		for _, id := range cited {
			get(id)
		}
	}
	return ev
}

// admitRequestEvidence adds to ev the cited ledger entries that belong to the
// same request but not to this step's own calls or inputs: entries of EARLIER
// stages or plan generations of this Execute (same root run, another run id) and
// entries of the run whose worker delegated this one (domain.ParentRunsFrom), or
// of the same conversation turn. Each is read through the ENFORCED reader under
// the run's read scope, so only what the person may read is admitted
// (T-20261109-staged-planner-names-earlier-stage-ledger: the stage planner named
// stage-1 entries in the step query, the worker fetched them with ledger_get, and
// D6 rejected 28 values as foreign_evidence). Entries of other steps of the same
// stage stay outside, as before (only dependencies count).
func (d *DAGExecutor) admitRequestEvidence(ctx context.Context, env *domain.AnswerEnvelope, ev *requirements.Evidence, obs *requirements.StepObservation) {
	if d.LedgerReader == nil || env == nil {
		return
	}
	var cited []string
	for _, r := range env.ItemResults {
		for _, v := range r.Values {
			cited = append(cited, v.EvidenceRef)
			if v.Type == domain.ValueRef {
				cited = append(cited, v.Text())
			}
		}
	}
	cited = append(cited, env.EvidenceRefs...)
	p := d.ledgerProvenance(ctx)
	root := domain.RootRunID(p.RunID)
	parents := map[string]bool{}
	for _, r := range domain.ParentRunsFrom(ctx) {
		parents[domain.RootRunID(string(r))] = true
	}
	scope := d.LedgerReadScope
	if scope == nil {
		scope = &domain.TagPredicate{Bypass: true}
	}
	rctx := domain.WithScope(ctx, scope)
	for _, id := range cited {
		id = strings.TrimSpace(id)
		if !domain.IsLedgerID(id) {
			continue
		}
		if _, have := ev.Entries[id]; have {
			continue
		}
		e, err := d.LedgerReader.Get(rctx, id)
		if err != nil || e == nil {
			continue // absent, or not readable under the person's access
		}
		switch e.Kind {
		case domain.LedgerToolResult, domain.LedgerComputed, domain.LedgerArtifact, domain.LedgerStepOutput:
		default:
			continue
		}
		er := domain.RootRunID(e.RunID)
		sameRequest := (er != "" && er == root && e.RunID != p.RunID) || // an earlier stage or generation
			(er != "" && parents[er]) || // the delegating run (any of its stages)
			(p.TurnID != "" && e.TurnID == p.TurnID && e.ConversationID == p.ConversationID) // the same turn
		if !sameRequest {
			continue
		}
		ev.Entries[id] = *e
		obs.EvidenceKinds[id] = e.Kind
	}
}

// depsOf is the transitive dependency set of step in the CURRENT plan (set by
// the coordinator before dispatch; see currentPlan).
func (d *DAGExecutor) depsOf(step int) []int {
	d.planMu.Lock()
	plan := d.currentPlan
	d.planMu.Unlock()
	if plan == nil || step < 0 || step >= len(plan.Steps) {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	stack := append([]int(nil), plan.Steps[step].DependsOn...)
	for len(stack) > 0 {
		j := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if j < 0 || j >= len(plan.Steps) || seen[j] {
			continue
		}
		seen[j] = true
		out = append(out, j)
		stack = append(stack, plan.Steps[j].DependsOn...)
	}
	return out
}

// isErrorResult reports whether a tool_result body is the error the tool executor
// records for a call that ran and failed ({"error": "..."}).
func isErrorResult(body []byte) bool {
	t := strings.TrimSpace(string(body))
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &m) != nil {
		return false
	}
	_, isErr := m["error"]
	return isErr && len(m) == 1
}

// expectFanOutMembers registers the members of a fan-out expansion with the
// coverage of every item its children answer (D5.6: one result per member).
func (d *DAGExecutor) expectFanOutMembers(plan *domain.ExecutionPlan, firstChild int) {
	if d.Requirements == nil || d.Requirements.Coverage == nil {
		return
	}
	members := map[string][]string{}
	for i := firstChild; i < len(plan.Steps); i++ {
		st := plan.Steps[i]
		if st.FanOutItem == "" {
			continue
		}
		for _, id := range st.Answers {
			members[id] = append(members[id], st.FanOutItem)
		}
	}
	for id, ms := range members {
		d.Requirements.Coverage.ExpectMembers(id, ms)
	}
}

// fanOutDependencyMembers are the fan-out members an ordinary step consumes: the
// FanOutItem of each fan-out child it depends on. Coverage requires a done write per
// member when the step's writes are per member (requirements.coverMembersByWrites).
func fanOutDependencyMembers(plan *domain.ExecutionPlan, st domain.Step) []string {
	if st.FanOutItem != "" {
		return nil
	}
	var out []string
	for _, dep := range st.DependsOn {
		if dep >= 0 && dep < len(plan.Steps) {
			out = appendUnique(out, plan.Steps[dep].FanOutItem)
		}
	}
	return out
}

// fanOutSourceOutput is the text a fan-out expands over. A source that answered
// items returned an envelope: its list is the answer_text when that is a JSON
// array, else the first list value it returned; a prose output is used as is.
func fanOutSourceOutput(out string) string {
	env, ok := domain.ParseAnswerEnvelope([]byte(out))
	if !ok {
		return out
	}
	if t := strings.TrimSpace(env.AnswerText); strings.HasPrefix(t, "[") && json.Valid([]byte(t)) {
		return t
	}
	for _, r := range env.ItemResults {
		for _, v := range r.Values {
			if v.Type == domain.ValueList {
				return string(v.Value)
			}
		}
	}
	return env.AnswerText
}

func appendUnique(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}
