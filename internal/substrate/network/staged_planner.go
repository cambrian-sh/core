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

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// The server's half of the ADR-0145 D11 staged planner (execution.plan.staged_planner;
// T-20261108-adr-0145-d11-staged-planner-returns-next-stage) and of D5a ask-back.
//
// The coverage-stage loop (coverage_stages.go) already re-plans after every stage
// until code decides the request is settled or stalled. Under the staged flag each
// planner call additionally gets a short fresh context (domain.StagePlanning): the
// checklist with item statuses, a compact index of the request's ledger entries,
// the earlier stages' step results by reference, today, and whether the person
// can be asked; it may read the ledger between its calls; and its reply is the
// next stage, a question for the person, or nothing more. The planner never
// declares done: "nothing more" closes the open items as gaps, and completion and
// stalls stay requirements.Coverage decisions.
//
// Ask-back (D5a): a question reaches the person only on the conversation path
// (the delegated run is bound to a chat turn) with execution.plan.ask_back_enabled
// and rounds left (ask_back_max_rounds). Every other path keeps today's behaviour
// by default: the planner is told it cannot ask, proceeds and states its
// assumptions, which are written into the checklist and shown in the result;
// ask_back_other_paths=report stops and returns the questions instead.

// stagedOn reports whether the staged planner runs (flag and checklist pipeline).
func (s *Server) stagedOn() bool {
	return s.ExecCfg.Plan.StagedPlanner && s.checklistOn()
}

// askBackRound is the ask-back round the request answers (the chat side sets
// HandoffKeyAskBackRound when it delegates the person's answer), 0 when none.
func askBackRound(meta map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(meta[domain.HandoffKeyAskBackRound]))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// stagedRun is what one Execute's staged planning needs across its stages.
type stagedRun struct {
	// prov is the run's ledger provenance (conversation, turn, owner); RunID is
	// the first stage's run id.
	prov domain.LedgerProvenance
	// conversation is true when the run was delegated from a chat turn.
	conversation bool
	round        int
	// stageRuns are the run ids every stage's entries are recorded under.
	stageRuns []domain.RunID
	// earlier accumulates the stages' step-result indexes.
	earlier []string
	// pendingRef is the ledger id of the InputRequired this request answers
	// (HandoffKeyAskBackPendingRef, kernel-stamped from the turn lease), "" when none.
	pendingRef string
}

func (s *Server) newStagedRun(ctx context.Context, meta map[string]string, run domain.RunID, sid domain.SessionID) *stagedRun {
	prov := s.runProvenance(ctx, meta, run, sid)
	r := &stagedRun{prov: prov, conversation: prov.TurnID != "", round: askBackRound(meta), stageRuns: []domain.RunID{run}}
	if ref := strings.TrimSpace(meta[domain.HandoffKeyAskBackPendingRef]); domain.IsLedgerID(ref) {
		r.pendingRef = ref
		if note := s.pendingQuestionsNote(ctx, r); note != "" {
			r.earlier = append(r.earlier, note)
		}
	}
	return r
}

// pendingQuestionsNote tells the first stage which earlier question set this
// request answers (D5a.2 resume): the recorded questions and the ledger id of the
// recorded state (its checklist and coverage, readable with ledger_get). Read
// under the planner's read scope; unreadable reads as nothing.
func (s *Server) pendingQuestionsNote(ctx context.Context, r *stagedRun) string {
	if !s.ledgerOn() {
		return ""
	}
	eff := s.plannerReadScope(ctx, r.prov.Owner)
	if eff == nil {
		return ""
	}
	e, err := s.Ledger.Reader.Get(domain.WithScope(ctx, eff), r.pendingRef)
	if err != nil || e == nil {
		return ""
	}
	var ir domain.InputRequired
	if json.Unmarshal(e.Body, &ir) != nil || len(ir.Questions) == 0 {
		return ""
	}
	return fmt.Sprintf("This request answers questions asked earlier (round %d; the earlier checklist and its coverage are ledger entry %s):\n%s\n"+
		"The person's answer is in the request; plan from it and do not ask the same questions again.",
		ir.Round, r.pendingRef, questionsText(ir.Questions))
}

// canAsk reports whether a question can reach the person now.
func (s *Server) canAsk(r *stagedRun) bool {
	return s.ExecCfg.Plan.AskBackEnabled && r.conversation && r.round < s.ExecCfg.Plan.AskBackRounds()
}

// stagePlanning builds the planner context of stage number.
func (s *Server) stagePlanning(ctx context.Context, r *stagedRun, number int) *domain.StagePlanning {
	sp := &domain.StagePlanning{
		Number:          number,
		CanAsk:          s.canAsk(r),
		EarlierStages:   strings.Join(r.earlier, "\n"),
		MaxLookupRounds: s.ExecCfg.Plan.StageReadRounds(),
	}
	if s.ledgerOn() {
		sp.LedgerIndex = s.ledgerIndex(ctx, r, s.ExecCfg.Plan.StageIndexEntries())
		if s.ToolExecutor != nil {
			sp.Lookup = s.plannerLedgerLookup(r)
		}
	}
	return sp
}

// plannerReadScope is the predicate the planner reads the ledger under: the
// caller's own (the agent that called Execute, resolved by the decision point
// exactly as GetContextNode resolves it), or — for a caller with no agent identity
// (an operator or transport call) — the run's own read scope, the kernel acting
// for the run's owner (the explicit §2.4 bypass with the owner fence, the scope
// the DAG executor reads its run under). nil ⇒ refused.
func (s *Server) plannerReadScope(ctx context.Context, owner string) *domain.TagPredicate {
	if callerAgentID(ctx) == "" {
		return runLedgerScope(owner)
	}
	eff, _ := s.authorizer().ReadFilter(ctx, artifactPrincipal(ctx), artifactSurface)
	return eff
}

// plannerLedgerLookup serves the planner's reads with the ledger kernel tools
// (ledger_get / ledger_find, the same code a worker calls) under the planner's
// read scope and the run's provenance (ledger_find's run / turn / conversation
// scopes resolve from it). Read-only: nothing else is served.
func (s *Server) plannerLedgerLookup(r *stagedRun) domain.LedgerLookupFunc {
	return func(ctx context.Context, l domain.LedgerLookup) string {
		var kt domain.KernelTool
		for _, t := range s.ToolExecutor.KernelTools {
			if t.Spec().Name == l.Tool {
				kt = t
			}
		}
		if kt == nil || (l.Tool != "ledger_get" && l.Tool != "ledger_find") {
			return fmt.Sprintf("refused: %q is not a ledger read", l.Tool)
		}
		eff := s.plannerReadScope(ctx, r.prov.Owner)
		if eff == nil {
			return "refused: no read access to the ledger"
		}
		rctx := domain.WithScope(domain.WithLedgerProvenance(ctx, r.prov), eff)
		out, err := kt.Call(rctx, domain.KernelToolCall{AgentID: callerAgentID(ctx), ArgsJSON: l.Arguments})
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)
	}
}

// ledgerIndexPreview bounds one index line's preview.
const ledgerIndexPreview = 160

// ledgerIndex is the compact index of the request's ledger entries the staged
// planner sees: the turn's entries when the run was delegated from a chat turn
// (every stage, and earlier delegations of the same turn), else every stage run's.
// One line per entry, oldest first, at most limit lines.
func (s *Server) ledgerIndex(ctx context.Context, r *stagedRun, limit int) string {
	eff := s.plannerReadScope(ctx, r.prov.Owner)
	if eff == nil {
		return ""
	}
	rctx := domain.WithScope(ctx, eff)
	var rows []domain.LedgerEntry
	seen := map[string]bool{}
	add := func(q domain.LedgerQuery) {
		q.Limit = limit + 1
		got, err := s.Ledger.Reader.Find(rctx, q)
		if err != nil {
			slog.Warn("ADR-0145 D11: ledger index not listed", "err", err)
			return
		}
		for _, e := range got {
			if !seen[e.ID] {
				seen[e.ID] = true
				rows = append(rows, e)
			}
		}
	}
	if r.prov.TurnID != "" {
		add(domain.LedgerQuery{TurnID: r.prov.TurnID})
	} else {
		for _, run := range r.stageRuns {
			add(domain.LedgerQuery{RunID: string(run)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	var b strings.Builder
	for i, e := range rows {
		if i >= limit {
			fmt.Fprintf(&b, "... %d more entries: search them with ledger_find\n", len(rows)-limit)
			break
		}
		tool := e.ToolName
		if tool == "" {
			tool = "-"
		}
		preview := strings.Join(strings.Fields(e.Preview), " ")
		if rs := []rune(preview); len(rs) > ledgerIndexPreview {
			preview = string(rs[:ledgerIndexPreview]) + "…"
		}
		fmt.Fprintf(&b, "%s %s %s step %d run %s: %s\n", e.ID, e.Kind, tool, e.StepIndex, e.RunID, preview)
	}
	return strings.TrimRight(b.String(), "\n")
}

// errStageQuestion is returned by planStage when the planner asked the person
// and the question can reach them: the run stops with an InputRequired.
type errStageQuestion struct{ ir *domain.InputRequiredError }

func (e *errStageQuestion) Error() string { return e.ir.Error() }

// planStage plans one stage under the staged arm. It returns the plan; or an
// *errStageQuestion when the planner asked and the person can be asked (or the
// path reports questions); or domain.ErrNoFurtherStage; or another error. A
// question on a path that cannot ask is answered by planning again with the
// person marked unreachable (the planner proceeds and states assumptions); a
// second question there ends the stage as "nothing more" with the questions as
// the reason, never a guess made in code.
func (s *Server) planStage(ctx context.Context, planner Planner, userInput string, reqs *requestRequirements,
	r *stagedRun, number int, cov *domain.CoverageStage) (*domain.ExecutionPlan, error) {
	sp := s.stagePlanning(ctx, r, number)
	pctx := ctx
	if cov != nil {
		pctx = domain.WithCoverageStage(pctx, cov)
	}
	plan, err := s.planChecked(domain.WithStagePlanning(pctx, sp), planner, userInput)
	s.addAssumptions(reqs, sp.Assumptions())
	var ir *domain.InputRequiredError
	if !errors.As(err, &ir) {
		return plan, err
	}
	if sp.CanAsk || (!r.conversation && s.ExecCfg.Plan.AskBackReportsOnOtherPaths()) {
		return nil, &errStageQuestion{ir: ir}
	}
	slog.Info("ADR-0145 D5a: the planner asked but the person cannot be asked; planning again with stated assumptions",
		"stage", number, "conversation", r.conversation, "round", r.round)
	sp2 := s.stagePlanning(ctx, r, number)
	sp2.CanAsk = false
	plan, err = s.planChecked(domain.WithStagePlanning(pctx, sp2), planner, userInput+
		"\n\nThe person cannot be asked these questions; choose the most reasonable reading, plan it and state each assumption:\n"+
		questionsText(ir.Questions))
	s.addAssumptions(reqs, sp2.Assumptions())
	if errors.As(err, &ir) {
		return nil, fmt.Errorf("%w: input required but the person cannot be asked: %s", domain.ErrNoFurtherStage, strings.Join(questionList(ir.Questions), " | "))
	}
	return plan, err
}

// addAssumptions writes the planner's stated assumptions into the checklist
// (D5a.3), once each.
func (s *Server) addAssumptions(reqs *requestRequirements, as []string) {
	if reqs == nil || len(as) == 0 {
		return
	}
	for _, a := range as {
		dup := false
		for _, have := range reqs.checklist.Assumptions {
			if have == a {
				dup = true
				break
			}
		}
		if !dup {
			reqs.checklist.Assumptions = append(reqs.checklist.Assumptions, a)
		}
	}
}

func questionList(qs []domain.InputQuestion) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Question)
	}
	return out
}

// questionsText renders questions one per line for the payload and the planner.
func questionsText(qs []domain.InputQuestion) string {
	var b strings.Builder
	for _, q := range qs {
		fmt.Fprintf(&b, "- %s [%s] %s", q.ID, q.Kind, q.Question)
		if len(q.Options) > 0 {
			fmt.Fprintf(&b, " (options: %s)", strings.Join(q.Options, "; "))
		}
		if len(q.Items) > 0 {
			fmt.Fprintf(&b, " (blocks %s)", strings.Join(q.Items, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// inputRequiredOf builds the structured result of a run that stopped to ask, and
// records it (with the partial coverage) in the ledger so the next turn resumes
// from that state by reference.
func (s *Server) inputRequiredOf(ctx context.Context, q *errStageQuestion, run domain.RunID, stage int,
	reqs *requestRequirements, r *stagedRun) *domain.InputRequired {
	ir := &domain.InputRequired{Version: domain.InputRequiredVersion, RunID: string(run), Stage: stage,
		Questions: q.ir.Questions, Reason: q.ir.Reason, Round: r.round + 1}
	if reqs != nil {
		rep := reqs.coverage.Snapshot()
		if b, err := json.Marshal(struct {
			Checklist *domain.RequirementChecklist `json:"checklist"`
			Items     any                          `json:"items"`
			Stages    int                          `json:"stages"`
		}{reqs.checklist, rep.Items, rep.Stages}); err == nil {
			ir.Requirements = b
		}
	}
	if s.ledgerOn() {
		if b, err := json.Marshal(ir); err == nil {
			p := domain.LedgerProvenanceFromContext(ctx)
			p.StepIndex = -1
			if ent, rerr := s.Ledger.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerComputed, Provenance: p,
				ToolName: "input_required", Writer: domain.SystemPrincipal, ContentType: "application/json", Body: b}); rerr == nil {
				ir.LedgerRef = ent.ID
			} else {
				slog.Warn("ADR-0145 D5a: input-required state not recorded to the ledger", "err", rerr)
			}
		}
	}
	return ir
}

// inputRequiredText is the payload text of a run that stopped to ask.
func inputRequiredText(ir *domain.InputRequired) string {
	return "INPUT REQUIRED (the work stopped to ask the person; ask these in the person's language, then delegate the answer):\n" +
		questionsText(ir.Questions)
}

// attachInputRequired stamps ir on the response metadata and on the structured
// result already there (domain.ExecuteResult.InputRequired).
func attachInputRequired(meta map[string]string, ir *domain.InputRequired) {
	if meta == nil || ir == nil {
		return
	}
	b, err := json.Marshal(ir)
	if err != nil {
		return
	}
	meta[domain.HandoffKeyInputRequired] = string(b)
	var res domain.ExecuteResult
	if json.Unmarshal([]byte(meta[domain.HandoffKeyExecuteResult]), &res) != nil {
		return
	}
	res.InputRequired = ir
	if b2, err := json.Marshal(res); err == nil {
		meta[domain.HandoffKeyExecuteResult] = string(b2)
	}
}

// planFirstStage plans stage 1 under the staged arm, before the run starts. When
// the planner asks a question that can reach the person, or has nothing to plan,
// the Execute ends here: early is the response (the checklist and the question or
// gaps recorded under a fresh run id).
func (s *Server) planFirstStage(ctx context.Context, planner Planner, userInput string, in *pb.Handoff,
	sid domain.SessionID, reqs *requestRequirements) (plan *domain.ExecutionPlan, r *stagedRun, early *pb.Handoff, err error) {
	run := domain.RunID(newPlanID())
	r = s.newStagedRun(ctx, in.GetMetadata(), run, sid)
	plan, err = s.planStage(ctx, planner, userInput, reqs, r, 1, nil)
	var q *errStageQuestion
	switch {
	case errors.As(err, &q):
	case errors.Is(err, domain.ErrNoFurtherStage):
	default:
		return plan, r, nil, err
	}
	rctx := domain.WithLedgerProvenance(ctx, r.prov)
	s.recordChecklist(rctx, reqs)
	meta := map[string]string{}
	var payload string
	var ir *domain.InputRequired
	if q != nil {
		ir = s.inputRequiredOf(rctx, q, run, 1, reqs, r)
		payload = inputRequiredText(ir) + "\n\n" + coverageText(reqs)
	} else {
		reqs.coverage.EndStage()
		reqs.coverage.CloseGaps("the planner found nothing to plan: " + err.Error())
		payload = coverageText(reqs)
	}
	if sid != "" {
		meta["_session_id"] = string(sid)
	}
	s.stampExecuteResult(meta, payload, run, sid, nil, nil, nil)
	s.attachCoverage(rctx, meta, reqs)
	attachInputRequired(meta, ir)
	slog.Info("ADR-0145 D11: Execute ends before any stage ran", "input_required", ir != nil, "run", run)
	return nil, r, &pb.Handoff{Id: in.Id, FromAgent: "orchestrator", ToAgent: "user",
		Payload: &pb.Object{Data: []byte(payload)}, Metadata: meta}, nil
}

// stageStoppedPartway turns a stage that stopped part-way (a step failed) into a
// stage result under the checklist arms: its completed steps' outputs and their
// coverage stand, and the next stage plans the open items — instead of a mid-plan
// replan through replan_cap, which emits no answers, inputs or for_each
// (T-20261108-replanner-replan-cap-does-not-emit-answers). A refusal the
// effect check made stays a question for the user (effectRefusalClarification).
// ok is false for any other error.
func stageStoppedPartway(err error) (map[string]string, bool) {
	var pe *executer.PartialPlanError
	if !errors.As(err, &pe) || pe.Context == nil {
		return nil, false
	}
	var noEffect *domain.NoEffectMatchError
	if errors.As(pe.LastError, &noEffect) {
		return nil, false
	}
	report := fmt.Sprintf("The stage stopped at step %d: %v", pe.FailedStep, pe.LastError)
	if strings.TrimSpace(pe.Report) != "" {
		report += "\n" + pe.Report
	}
	pe.Context[finalResultKey] = report
	return pe.Context, true
}
