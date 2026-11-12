package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/planvalidate"
	"github.com/cambrian-sh/core/internal/requirements"
)

// The server's half of ADR-0145 phase 2 (T-20261108-adr-0145-phase-2-requirement-checklist-with):
// the requirement checklist is derived before planning (D5), the D7 validator
// gates every plan, and after a plan completes, code decides whether the request
// is done (every item final), stalled (a stage changed nothing: the open items
// close as gaps) or needs another stage for exactly the open items (D5.5). There
// is no turn or spend budget in this loop (decisions/no-turn-budgets-for-now-
// terminate-on-progress.md): only execution.plan.coverage_max_stages, a runaway cap.
//
// Legacy path (execution.plan.checklist_enabled off): no checklist, one plan, the
// DAG-only validatePlan unless execution.plan.plan_validator is on. The legacy
// single-plan path is deleted with the flag once the checklist defaults on
// (T-20261108-delete-the-pre-checklist-prose-step-path-once).

// ChecklistDeriver derives a request's checklist (requirements.Deriver).
type ChecklistDeriver interface {
	Derive(ctx context.Context, request string) domain.RequirementChecklist
}

// requestRequirements is one Execute's checklist state.
type requestRequirements struct {
	checklist *domain.RequirementChecklist
	coverage  *requirements.Coverage
	exec      *executer.StepRequirements
	// earlier are the finished stages' completed steps under their e<K> labels
	// (addEarlier), in order.
	earlier []domain.EarlierResult
}

// checklistOn reports whether the D5 pipeline runs (flag, deriver and ledger).
func (s *Server) checklistOn() bool {
	return s.ExecCfg.Plan.ChecklistEnabled && s.Checklist != nil && s.ledgerOn()
}

// deriveRequirements derives the checklist from the user's own words in the
// request (the <user_request> region a chat delegation carries, else the whole
// request) and starts its coverage.
func (s *Server) deriveRequirements(ctx context.Context, rawInput string) *requestRequirements {
	cl := s.Checklist.Derive(ctx, domain.UserRequestText(rawInput))
	cov := requirements.NewCoverage(cl)
	slog.Info("ADR-0145 D5: requirement checklist derived", "items", len(cl.Items), "fallback", cl.Fallback, "dropped", len(cl.Dropped))
	return &requestRequirements{checklist: &cl, coverage: cov}
}

// planCallLimit is the replans a plan may spend: max_replan_attempts, plus the
// D7 validator's repair rounds when it is on.
func (s *Server) planCallLimit() int {
	n := s.ExecCfg.Plan.MaxReplanAttempts
	if s.ExecCfg.Plan.PlanValidator {
		n += s.ExecCfg.Plan.ValidatorRepairs()
	}
	if s.stagedOn() {
		n++ // the staged planner's re-plan with stated assumptions (planStage)
	}
	return n
}

// planChecked plans userInput and validates the plan: the D7 validator when
// execution.plan.plan_validator is on (structured violations back to the planner,
// up to plan_validator_max_repairs rounds), else the legacy DAG-only check.
func (s *Server) planChecked(ctx context.Context, planner Planner, userInput string) (*domain.ExecutionPlan, error) {
	if !s.ExecCfg.Plan.PlanValidator {
		return planWithValidation(ctx, planner, userInput)
	}
	checklist := domain.RequirementChecklistFrom(ctx)
	sources := []string{userInput, s.Calendar.Block()}
	if checklist != nil {
		sources = append(sources, checklist.Request)
	}
	var settled []string
	var earlier []domain.EarlierResult
	if st := domain.CoverageStageFrom(ctx); st != nil {
		sources = append(sources, st.Earlier)
		settled = st.Settled
		earlier = st.EarlierResults
	}
	// ADR-0145 D11: a staged plan is ONE stage — it answers open items only, at
	// least one — and the dates it may copy include what the ledger index and the
	// earlier stages show.
	sp := domain.StagePlanningFrom(ctx)
	if sp != nil {
		sources = append(sources, sp.LedgerIndex, sp.EarlierStages)
	}
	vocab := stepCacheVocabulary(s.ExecCfg.Capability)
	input := userInput
	for round := 0; ; round++ {
		plan, err := planner.GetExecutionPlan(ctx, input)
		if err != nil {
			return nil, err
		}
		vs := planvalidate.Validate(planvalidate.Input{Plan: plan, Checklist: checklist, Vocabulary: vocab, DateSources: sources,
			Staged: sp != nil, Settled: settled, Earlier: earlier, Written: s.writtenTargets(ctx, planRunID(ctx))})
		if len(vs) == 0 {
			return plan, nil
		}
		slog.Warn("ADR-0145 D7: plan rejected by the validator", "round", round, "violations", len(vs), "first", vs[0])
		if round >= s.ExecCfg.Plan.ValidatorRepairs() {
			return nil, fmt.Errorf("planner produced an invalid plan after %d repair rounds: %w", round,
				&PlanValidationError{Signal: planvalidate.Render(vs)})
		}
		input = userInput + "\n\n<plan_violations>\n" + planvalidate.Render(vs) + "\n</plan_violations>\n" +
			"The previous plan broke the rules listed in plan_violations. Write the whole plan again so that none of them applies."
	}
}

// recordChecklist stores the checklist in the run ledger (D5: "the checklist is
// stored in the ledger") as a computed entry of the run.
func (s *Server) recordChecklist(ctx context.Context, r *requestRequirements) {
	if r == nil || !s.ledgerOn() {
		return
	}
	b, err := json.Marshal(r.checklist)
	if err != nil {
		return
	}
	p := domain.LedgerProvenanceFromContext(ctx)
	p.StepIndex = -1
	ent, err := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerComputed, Provenance: p,
		ToolName: "requirement_checklist", Writer: domain.SystemPrincipal, ContentType: "application/json", Body: b})
	if err != nil {
		slog.Warn("ADR-0145 D5: checklist not recorded to the ledger", "err", err)
		return
	}
	r.checklist.LedgerRef = ent.ID
}

// recordCoverage stores the final coverage report as a computed entry derived
// from every entry its accepted values cite (so it is never easier to read than
// its evidence), and returns its JSON for the structured result.
func (s *Server) recordCoverage(ctx context.Context, r *requestRequirements) json.RawMessage {
	rep := r.coverage.Snapshot()
	out := struct {
		Checklist *domain.RequirementChecklist `json:"checklist"`
		requirements.Report
		LedgerRef string `json:"ledger_ref,omitempty"`
	}{Checklist: r.checklist, Report: rep}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	if s.ledgerOn() {
		var derived []string
		for _, it := range rep.Items {
			derived = append(derived, it.Evidence...)
		}
		p := domain.LedgerProvenanceFromContext(ctx)
		p.StepIndex = -1
		if ent, rerr := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerComputed, Provenance: p,
			ToolName: "requirement_coverage", Writer: domain.SystemPrincipal, DerivedFrom: derived,
			ContentType: "application/json", Body: b}); rerr == nil {
			out.LedgerRef = ent.ID
			if b2, merr := json.Marshal(out); merr == nil {
				b = b2
			}
		}
	}
	return b
}

// coverageText renders the coverage for the text payload, so a caller that reads
// only text (the chat agent today) sees every item's outcome and every gap.
func coverageText(r *requestRequirements) string {
	rep := r.coverage.Snapshot()
	var b strings.Builder
	b.WriteString("REQUIREMENT COVERAGE (decided by the kernel from the run ledger):\n")
	for _, it := range rep.Items {
		fmt.Fprintf(&b, "- %s [%s] %s: %s", it.Item.ID, it.Item.Type, it.Item.Text, it.Status)
		if it.Reason != "" {
			fmt.Fprintf(&b, " (%s)", it.Reason)
		}
		for _, v := range it.Values {
			member := ""
			if v.Member != "" {
				member = " for " + v.Member
			}
			refused := ""
			if v.Refused {
				refused = ", not accepted"
			}
			fmt.Fprintf(&b, "\n    %s%s = %s [%s, evidence %s%s]", v.Name, member, v.Text(), v.Type, v.EvidenceRef, refused)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// effectsOfTool maps a tool to the constraint terms it falls under: every term of
// execution.tools.constraint_classes whose classes include one of the tool's
// ADR-0086 classes (D14: constraints hold by tool class).
func (s *Server) effectsOfTool() func(domain.PlanAction) []string {
	terms, err := s.ExecCfg.Tools.ConstraintTerms()
	if err != nil || s.ToolExecutor == nil || s.ToolExecutor.Registry == nil {
		return nil
	}
	return func(a domain.PlanAction) []string {
		t, ok := s.ToolExecutor.Registry.Get(a.Tool)
		if !ok {
			return nil
		}
		var out []string
		for term, classes := range terms {
			for _, c := range classes {
				// ADR-0145 D14.6: a class the call's step needed for the request's
				// own write item is not a constraint violation.
				if t.HasEffect(c) && !containsToolEffect(a.Carries, c) {
					out = append(out, term)
					break
				}
			}
		}
		sort.Strings(out)
		return out
	}
}

func containsToolEffect(xs []domain.ToolEffect, x domain.ToolEffect) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// planRunID is the root run of the Execute a planner call belongs to (its ledger
// provenance), "" when unknown — the write record's scope then falls back to the
// conversation turn alone.
func planRunID(ctx context.Context) domain.RunID {
	return domain.RunID(domain.RootRunID(domain.LedgerProvenanceFromContext(ctx).RunID))
}

// stepActionsOf returns the kernel's records of one step's calls in the CURRENT
// stage's run: its side-effecting calls and its refused reads, of the step's plan
// generation (indices restart after a replan).
func (s *Server) stepActionsOf(run *domain.RunID) func(generation, step int) executer.StepRecords {
	return func(generation, step int) executer.StepRecords {
		var out executer.StepRecords
		for _, a := range s.planActions.snapshot(*run) {
			if a.Step == step && a.Generation == generation {
				out.Actions = append(out.Actions, a)
			}
		}
		for _, a := range s.planActions.deniedReads(*run) {
			if a.Step == step && a.Generation == generation {
				out.Denials = append(out.Denials, a)
			}
		}
		return out
	}
}

// addEarlier labels a finished stage's completed steps e<K> (K continuing across
// the request's stages) and returns the new entries. The labels are the one way a
// later stage's plan binds an earlier result in inputs (domain.InputRef; the
// validator accepts exactly these, the executor resolves them;
// T-20261112-staged-planner-binds-earlier-stage-results-with).
func (r *requestRequirements) addEarlier(stage int, refs []domain.ExecuteStepRef) []domain.EarlierResult {
	var out []domain.EarlierResult
	for _, ref := range refs {
		if ref.Status != domain.ExecuteStepCompleted {
			continue
		}
		e := domain.EarlierResult{Label: domain.EarlierLabel(len(r.earlier) + 1), Stage: stage, Step: ref.Step,
			Query: ref.Query, Ref: ref.Ref, Preview: ref.Preview}
		r.earlier = append(r.earlier, e)
		out = append(out, e)
	}
	return out
}

// earlierStagesIndex is the compact view of one earlier stage a coverage stage's
// planner sees: each completed step under its e<K> label (what inputs bind), with
// its ledger reference and preview (references, not data; the workers read the
// entries by id).
func earlierStagesIndex(stage int, es []domain.EarlierResult) string {
	if len(es) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Results of stage %d (bind one in inputs by its label, e.g. %q or %q; workers read the entry with ledger_get):\n",
		stage, es[0].Label, es[0].Label+".<name>")
	for _, e := range es {
		fmt.Fprintf(&b, "- %s = stage %d step %d %q -> %s: %s\n", e.Label, e.Stage, e.Step, e.Query, e.Ref, e.Preview)
	}
	return strings.TrimRight(b.String(), "\n")
}

// attachCoverage records the final coverage to the ledger and adds it to the
// structured Execute result already stamped on meta (domain.ExecuteResult.Requirements).
func (s *Server) attachCoverage(ctx context.Context, meta map[string]string, r *requestRequirements) {
	raw := s.recordCoverage(ctx, r)
	if raw == nil || meta == nil {
		return
	}
	var res domain.ExecuteResult
	if err := json.Unmarshal([]byte(meta[domain.HandoffKeyExecuteResult]), &res); err != nil {
		return
	}
	res.Requirements = raw
	if b, err := json.Marshal(res); err == nil {
		meta[domain.HandoffKeyExecuteResult] = string(b)
	}
}

// mergeStageStepRefs appends a coverage stage's step index to the run's (in
// meta under executer.PlanStepsKey), marking the first plan's steps stage 1.
func mergeStageStepRefs(meta map[string]string, stageRefs []domain.ExecuteStepRef, stage int) {
	refs := executer.PlanStepRefsFrom(meta)
	for i := range refs {
		if refs[i].Stage == 0 {
			refs[i].Stage = 1
		}
	}
	for _, r := range stageRefs {
		r.Stage = stage
		refs = append(refs, r)
	}
	if b, err := json.Marshal(refs); err == nil {
		meta[executer.PlanStepsKey] = string(b)
	}
}

// stageLoop is what the coverage-stage loop needs from one Execute.
type stageLoop struct {
	reqs        *requestRequirements
	executionID domain.RunID
	// stepRun is the Execute's variable the current stage's leases and action
	// records are bound to (the step closure reads it); the loop moves it.
	stepRun     *domain.RunID
	ctx         context.Context // the Execute's ctx (with the run's ledger provenance)
	planCtx     context.Context
	userInput   string
	initialCtx  map[string]string
	newExecutor func(domain.RunID) *executer.DAGExecutor
	stepFn      executer.StepFunc
	// staged is the staged planner's state (nil ⇒ the phase-2 coverage stages).
	staged *stagedRun
	// stopped is the first stage's error when it stopped part-way (nil otherwise):
	// a failure that needs re-planning reaches the next stage's planner
	// (stageReplan, step_recovery.go).
	stopped error
}

// stageOutcome is what the loop produced.
type stageOutcome struct {
	reports []string
	actions []domain.PlanAction
	// askBack is set when a later stage's planner asked the person (D5a).
	askBack *domain.InputRequired
}

// runStages is ADR-0145 D5.5 / D11: code decides whether the request is done.
// After each stage: every item final ⇒ done; no item changed status or gained
// evidence ⇒ stall, the open items close as gaps; otherwise the next stage is
// planned for exactly the open items. The only other stop is the runaway cap.
// A stage that stopped part-way (a step failed) is a stage like any other: what
// it settled stands and the next stage plans the rest (there is no mid-stage
// replan under the checklist). Under the staged planner the planner may also
// answer "nothing more" (the open items close as gaps) or ask the person.
func (s *Server) runStages(l stageLoop, masterCtx map[string]string) stageOutcome {
	reqs := l.reqs
	out := stageOutcome{reports: []string{masterCtx[finalResultKey]}}
	lastRefs := executer.PlanStepRefsFrom(masterCtx)
	lastEarlier := reqs.addEarlier(1, lastRefs)
	if l.staged != nil {
		l.staged.stageRuns[0] = l.executionID
		l.staged.prov.RunID = string(l.executionID)
		l.staged.earlier = append(l.staged.earlier, earlierStagesIndex(1, lastEarlier))
	}
	replan := stageReplan(l.stopped, 1)
	replans := 0
	for {
		stageActions := s.planActions.snapshot(*l.stepRun)
		out.actions = append(out.actions, stageActions...)
		reqs.coverage.ObserveActions(stageActions, s.effectsOfTool())
		progress := reqs.coverage.EndStage()
		if reqs.coverage.Settled() {
			break
		}
		if !progress && replan != "" && replans < s.ExecCfg.Plan.MaxReplanAttempts {
			// A stage that changed nothing because a step needs re-planning is not a
			// stall: the planner re-plans that step (bounded by the replan ladder).
			replans++
			progress = true
		}
		if !progress {
			reqs.coverage.CloseGaps("stalled: the last stage changed no item")
			break
		}
		if reqs.coverage.Stages() >= s.ExecCfg.Plan.CoverageStages() {
			reqs.coverage.CloseGaps("runaway cap execution.plan.coverage_max_stages reached")
			break
		}
		stage := reqs.coverage.NextStage(reqs.coverage.Stages() + 1)
		stage.Earlier = earlierStagesIndex(stage.Number-1, lastEarlier)
		stage.EarlierResults = append([]domain.EarlierResult(nil), reqs.earlier...)
		if replan != "" {
			stage.Earlier = strings.TrimSpace(stage.Earlier + "\n" + replan)
		}
		var next *domain.ExecutionPlan
		var perr error
		planner := newPlanCallBudget(s.planCallLimit()).planner(s.Planner)
		if l.staged != nil {
			if replan != "" {
				// For this planner call only: the failure belongs to the stage just run.
				l.staged.earlier = append(l.staged.earlier, replan)
			}
			next, perr = s.planStage(l.planCtx, planner, l.userInput, reqs, l.staged, stage.Number, stage)
			if replan != "" {
				l.staged.earlier = l.staged.earlier[:len(l.staged.earlier)-1]
			}
			var q *errStageQuestion
			switch {
			case errors.As(perr, &q):
				// The person is asked; the open items stay open (the answer resumes them).
				out.askBack = s.inputRequiredOf(l.ctx, q, l.executionID, stage.Number, reqs, l.staged)
				out.reports = append(out.reports, inputRequiredText(out.askBack))
			case errors.Is(perr, domain.ErrNoFurtherStage):
				reqs.coverage.CloseGaps("the planner had nothing more to plan for them: " + perr.Error())
			}
			if q != nil || errors.Is(perr, domain.ErrNoFurtherStage) {
				break
			}
		} else {
			next, perr = s.planChecked(domain.WithCoverageStage(l.planCtx, stage), planner, l.userInput)
		}
		if perr != nil {
			reqs.coverage.CloseGaps("the next stage could not be planned: " + perr.Error())
			break
		}
		if *l.stepRun != l.executionID {
			s.planActions.end(*l.stepRun) // a finished stage's calls are in out.actions
		}
		*l.stepRun = domain.RunID(fmt.Sprintf("%s-s%d", l.executionID, stage.Number))
		s.planActions.begin(*l.stepRun)
		if l.staged != nil {
			l.staged.stageRuns = append(l.staged.stageRuns, *l.stepRun)
		}
		sprov := domain.LedgerProvenanceFromContext(l.ctx)
		sprov.RunID = string(*l.stepRun)
		stageCtx := domain.WithLedgerProvenance(l.planCtx, sprov)
		// The finished stage's accepted values move under their e<K> labels before
		// the new stage's step indices restart.
		reqs.exec.ArchiveStage(lastEarlier)
		reqs.exec.ResetStage()
		slog.Info("ADR-0145 D5.5: coverage stage", "stage", stage.Number, "open", len(stage.Open), "steps", len(next.Steps), "staged", l.staged != nil)
		stageMaster, serr := l.newExecutor(*l.stepRun).ExecuteFrom(stageCtx, next, l.initialCtx, l.stepFn, 0)
		replan = stageReplan(serr, stage.Number)
		if serr != nil {
			m, ok := stageStoppedPartway(serr)
			if !ok {
				var pe *executer.PartialPlanError
				if errors.As(serr, &pe) {
					out.reports = append(out.reports, pe.Report)
				}
				out.actions = append(out.actions, s.planActions.snapshot(*l.stepRun)...)
				reqs.coverage.ObserveActions(s.planActions.snapshot(*l.stepRun), s.effectsOfTool())
				reqs.coverage.EndStage()
				reqs.coverage.CloseGaps("stage " + strconv.Itoa(stage.Number) + " stopped: " + serr.Error())
				break
			}
			slog.Info("ADR-0145 D5.5: stage stopped part-way; its results stand and the next stage plans the rest",
				"stage", stage.Number, "err", serr)
			stageMaster = m
		}
		out.reports = append(out.reports, stageMaster[finalResultKey])
		// The structured result lists every stage's steps; the stage's own
		// step_N_* keys stay in its context (indices restart per stage).
		lastRefs = executer.PlanStepRefsFrom(stageMaster)
		mergeStageStepRefs(masterCtx, lastRefs, stage.Number)
		lastEarlier = reqs.addEarlier(stage.Number, lastRefs)
		if l.staged != nil {
			l.staged.earlier = append(l.staged.earlier, earlierStagesIndex(stage.Number, lastEarlier))
		}
	}
	if *l.stepRun != l.executionID {
		s.planActions.end(*l.stepRun)
	}
	return out
}
