// Package judgereplay_test replays judge cases recorded from live runs through the
// CURRENT deterministic judges of ADR-0145, offline: the checklist anchoring
// (requirements.Deriver over the recorded model replies), envelope validation
// (requirements.ValidateEnvelope), the coverage item state machine
// (requirements.Coverage) and the plan validator (planvalidate.Validate).
//
// It is test code, never shipped: nothing here is linked into a binary, and it
// carries no benchmark logic (decisions/no-benchmark-in-oss.md). The cases are
// extracted, labelled and scored by cambrian-benchmarks
// (src/cambrian_bench/judgereplay); this file only feeds recorded inputs to the
// real judge code and writes what the judges decide. With CAMBRIAN_JUDGE_CASES
// unset the test is skipped, so `go test ./...` is unaffected.
//
//	CAMBRIAN_JUDGE_CASES  a directory of case files (*.json, one request each)
//	CAMBRIAN_JUDGE_OUT    the verdicts file to write (JSON lines); default
//	                      <cases>/verdicts.jsonl
//
// What is reconstructed here rather than read from the kernel's own state (the
// kernel keeps it in memory or in Postgres, the traces do not): the plan action
// ledger (from the recorded tool calls, with the target from the shipped
// WriteTargets rules and the side-effect class from the run's mcp.json tool
// policies), the step's citable ledger entries (own calls, the calls of its
// dependencies, step outputs, and any cited entry of the same request), and the
// step guard's Carries (domain.CarriedClasses over the open write items the step
// answers). The judges themselves are called as they are.
//
// T-20261112-no-offline-harness-re-measures-the-checklist.
package judgereplay_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/planvalidate"
	"github.com/cambrian-sh/core/internal/requirements"
)

// ── case file (written by cambrian-benchmarks' extractor) ───────────────────

type judgeCase struct {
	ID string `json:"id"`
	// Request is the user's own words the deriver saw (<request> of its input).
	Request string `json:"request"`
	// ChecklistReplies are the recorded requirement_checklist replies in order: the
	// first call, then the repair call when there was one.
	ChecklistReplies []string `json:"checklist_replies"`
	// MCPConfig is the run's mcp.json (tool effect policies).
	MCPConfig string `json:"mcp_config"`
	// WorldWrites are the files the world shows written by the request (absolute
	// paths, from the graders' fs_state evidence): the anchoring check asks whether
	// the derived targets accept them.
	WorldWrites []string    `json:"world_writes"`
	Stages      []caseStage `json:"stages"`
	// RequestGuards is execution.tools.request_guards as the run had it (its boot
	// log): the step guard, and with it PlanAction.Carries, exists only when on.
	// CAMBRIAN_JUDGE_REQUEST_GUARDS=on|off overrides it for a counterfactual.
	RequestGuards *bool `json:"request_guards,omitempty"`
}

type caseStage struct {
	Number int `json:"number"`
	// PlanCalls are the planner calls of the stage in order, with the verdict the
	// kernel recorded for each (accepted, rejected, unknown).
	PlanCalls   []planCall       `json:"plan_calls"`
	Generations []caseGeneration `json:"generations"`
}

type planCall struct {
	Reply        string `json:"reply"`
	Input        string `json:"input"`
	Recorded     string `json:"recorded"`
	RecordedRule string `json:"recorded_rule,omitempty"`
	Staged       bool   `json:"staged"`
	// Settled are the item ids the planner was told are settled (its input).
	Settled []string               `json:"settled,omitempty"`
	Earlier []domain.EarlierResult `json:"earlier,omitempty"`
}

type caseGeneration struct {
	PlanID string `json:"plan_id"`
	// PlanReply is the accepted plan's reply text.
	PlanReply string     `json:"plan_reply"`
	FanOuts   []fanOut   `json:"fan_outs,omitempty"`
	Steps     []caseStep `json:"steps"`
}

type fanOut struct {
	Step    int      `json:"step"`
	Members []string `json:"members"`
}

type caseStep struct {
	Index   int        `json:"index"`
	Query   string     `json:"query"`
	Outputs []string   `json:"outputs"`
	Calls   []toolCall `json:"calls"`
	// OutputRef is the step output's ledger id when the trace names it.
	OutputRef string `json:"output_ref,omitempty"`
	StartNs   int64  `json:"start_ns"`
	EndNs     int64  `json:"end_ns"`
}

type toolCall struct {
	Tool          string `json:"tool"`
	Args          string `json:"args"`
	Result        string `json:"result"`
	LedgerID      string `json:"ledger_id"`
	Error         string `json:"error,omitempty"`
	Denied        bool   `json:"denied,omitempty"`
	DenyReason    string `json:"deny_reason,omitempty"`
	ApprovalState string `json:"approval_state,omitempty"`
	// BodyMissing: the trace does not hold the result body (offloaded, never read
	// back); the entry is replayed as stored-truncated, so the deep check keeps
	// the value (requirements.ValidateEnvelope never checks past a cut body).
	BodyMissing bool `json:"body_missing,omitempty"`
}

// ── verdicts (read by cambrian-benchmarks' runner) ──────────────────────────

type verdict struct {
	ID        string                      `json:"id"`
	Error     string                      `json:"error,omitempty"`
	Checklist domain.RequirementChecklist `json:"checklist"`
	// CoverageChecklist is the checklist the recorded plans were built on (the
	// run's last checklist reply, anchored by the current code).
	CoverageChecklist domain.RequirementChecklist `json:"coverage_checklist"`
	RequestGuards     bool                        `json:"request_guards"`
	// InvalidTypeDrops are reply items code dropped outright for an unknown type
	// (no repair can fix them).
	InvalidTypeDrops []map[string]string `json:"invalid_type_drops,omitempty"`
	// TargetDrops are the declared targets anchorTargets dropped (its log).
	TargetDrops []map[string]string `json:"target_drops,omitempty"`
	// RepairUnrecorded: the current code wanted a repair call the run never made.
	RepairUnrecorded bool                `json:"repair_unrecorded,omitempty"`
	Anchoring        []anchorCheck       `json:"anchoring,omitempty"`
	Envelopes        []envelopeRow       `json:"envelopes,omitempty"`
	Stages           []stageRow          `json:"stages,omitempty"`
	Plans            []planRow           `json:"plans,omitempty"`
	Coverage         requirements.Report `json:"coverage"`
	Notes            []string            `json:"notes,omitempty"`
}

type anchorCheck struct {
	Item     string              `json:"item"`
	Type     string              `json:"type"`
	Targets  []domain.ItemTarget `json:"targets,omitempty"`
	Accepted []string            `json:"accepted,omitempty"`
	Refused  []string            `json:"refused,omitempty"`
}

type envelopeRow struct {
	Stage      int                        `json:"stage"`
	Generation int                        `json:"generation"`
	Step       int                        `json:"step"`
	Item       string                     `json:"item"`
	ItemType   string                     `json:"item_type"`
	Claimed    string                     `json:"claimed"`
	Sent       int                        `json:"sent"`
	Kept       int                        `json:"kept"`
	Violations []domain.EnvelopeViolation `json:"violations,omitempty"`
	NoEnvelope bool                       `json:"no_envelope,omitempty"`
}

type stageRow struct {
	Number   int               `json:"number"`
	Progress bool              `json:"progress"`
	Statuses map[string]string `json:"statuses"`
}

type planRow struct {
	Stage        int                      `json:"stage"`
	Call         int                      `json:"call"`
	Recorded     string                   `json:"recorded"`
	RecordedRule string                   `json:"recorded_rule,omitempty"`
	Replayed     string                   `json:"replayed"`
	Violations   []planvalidate.Violation `json:"violations,omitempty"`
	DecodeError  string                   `json:"decode_error,omitempty"`
}

// ── the test ────────────────────────────────────────────────────────────────

func TestJudgeReplay(t *testing.T) {
	dir := os.Getenv("CAMBRIAN_JUDGE_CASES")
	if dir == "" {
		t.Skip("CAMBRIAN_JUDGE_CASES not set: no recorded judge cases to replay")
	}
	out := os.Getenv("CAMBRIAN_JUDGE_OUT")
	if out == "" {
		out = filepath.Join(dir, "verdicts.jsonl")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no case files in %s (%v)", dir, err)
	}
	sort.Strings(files)
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	logs := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	defer slog.SetDefault(prev)

	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c judgeCase
		if err := json.Unmarshal(b, &c); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		v := replay(&c, logs)
		line, _ := json.Marshal(v)
		w.Write(line)
		w.WriteByte('\n')
	}
	t.Logf("replayed %d cases into %s", len(files), out)
}

func replay(c *judgeCase, logs *captureHandler) (v verdict) {
	v.ID = c.ID
	defer func() {
		if r := recover(); r != nil {
			v.Error = fmt.Sprintf("panic: %v", r)
		}
	}()
	terms, err := config.ToolsConfig{}.ConstraintTerms()
	if err != nil {
		v.Error = err.Error()
		return v
	}
	tools, err := loadToolPolicies(c.MCPConfig)
	if err != nil {
		v.Notes = append(v.Notes, "mcp config: "+err.Error())
	}
	writeTargets := config.DefaultWriteTargets()
	wt := writeTargets.Domain()

	// 1. Checklist anchoring: the real Deriver over the recorded replies, as the
	// current code would run them (it may want a repair the run never made).
	logs.reset()
	gen := &scripted{replies: c.ChecklistReplies}
	d := &requirements.Deriver{LLM: gen, Constraints: terms}
	cl := d.Derive(context.Background(), c.Request)
	v.Checklist = cl
	v.RepairUnrecorded = gen.exhausted
	v.TargetDrops = append(v.TargetDrops, logs.take("ADR-0145 D5: checklist item target dropped")...)
	v.InvalidTypeDrops = invalidTypeDrops(c.ChecklistReplies)

	// 1b. Anchoring against the world: would coverage accept the files the world
	// shows written, by the item's declared targets?
	for _, it := range cl.Items {
		if (it.Type != domain.RequirementWrite && it.Type != domain.RequirementDocument) || len(c.WorldWrites) == 0 {
			continue
		}
		ac := anchorCheck{Item: it.ID, Type: string(it.Type), Targets: it.Targets}
		for _, p := range c.WorldWrites {
			if acceptsWrite(it, p, wt) {
				ac.Accepted = append(ac.Accepted, p)
			} else {
				ac.Refused = append(ac.Refused, p)
			}
		}
		v.Anchoring = append(v.Anchoring, ac)
	}

	// The checklist the recorded plans were built on: the run's checklist came from
	// its LAST reply (a repair reply replaces the first one), anchored here by the
	// current code. Item ids line up with the recorded plans' answers only through
	// this one; the scorer checks the alignment with the recorded checklist.
	cl2 := cl
	if n := len(c.ChecklistReplies); n > 1 {
		logs.reset()
		d2 := &requirements.Deriver{LLM: &scripted{replies: c.ChecklistReplies[n-1:]}, Constraints: terms}
		cl2 = d2.Derive(context.Background(), c.Request)
	}
	v.CoverageChecklist = cl2
	guards := c.RequestGuards != nil && *c.RequestGuards
	if o := os.Getenv("CAMBRIAN_JUDGE_REQUEST_GUARDS"); o != "" {
		guards = o == "on"
	}
	v.RequestGuards = guards

	// 2-4. Plans, envelopes and coverage, stage by stage.
	cov := requirements.NewCoverage(cl2)
	requestEntries := map[string]domain.LedgerEntry{} // every entry of the request so far
	for _, st := range c.Stages {
		for ci, pc := range st.PlanCalls {
			v.Plans = append(v.Plans, validatePlan(st.Number, ci, pc, &cl2))
		}
		var stageActions []domain.PlanAction
		for gi, g := range st.Generations {
			plan, err := decodePlan(g.PlanReply)
			if err != nil {
				v.Notes = append(v.Notes, fmt.Sprintf("stage %d generation %d: plan not decoded: %v", st.Number, gi, err))
				continue
			}
			for _, fo := range g.FanOuts {
				if fo.Step < 0 || fo.Step >= len(plan.Steps) || plan.Steps[fo.Step].FanOutOver == nil {
					v.Notes = append(v.Notes, fmt.Sprintf("stage %d: fan-out at step %d not in the plan", st.Number, fo.Step))
					continue
				}
				first := len(plan.Steps)
				p2, err := domain.ExpandFanOut(plan, fo.Step, fo.Members, 10000)
				if err != nil {
					v.Notes = append(v.Notes, fmt.Sprintf("stage %d: fan-out at step %d: %v", st.Number, fo.Step, err))
					continue
				}
				plan = p2
				// expectFanOutMembers (executer): register the members with every item
				// the children answer.
				members := map[string][]string{}
				for i := first; i < len(plan.Steps); i++ {
					for _, id := range plan.Steps[i].Answers {
						members[id] = append(members[id], plan.Steps[i].FanOutItem)
					}
				}
				for id, ms := range members {
					cov.ExpectMembers(id, ms)
				}
			}
			// Entries of this generation, by step.
			byStep := map[int][]domain.LedgerEntry{}
			for _, s := range g.Steps {
				for _, call := range s.Calls {
					if e, ok := entryOf(call, s.Index); ok {
						byStep[s.Index] = append(byStep[s.Index], e)
					}
				}
				if s.OutputRef != "" && len(s.Outputs) > 0 {
					requestEntries[s.OutputRef] = domain.LedgerEntry{ID: s.OutputRef, Kind: domain.LedgerStepOutput,
						StepIndex: s.Index, Body: []byte(s.Outputs[len(s.Outputs)-1])}
				}
			}
			// Steps in the kernel's order: the guard's Carries is computed when a step
			// is dispatched, coverage observes the step when it finishes.
			type event struct {
				at     int64
				finish bool
				s      caseStep
			}
			var evs []event
			for _, s := range g.Steps {
				if s.Index < 0 || s.Index >= len(plan.Steps) {
					v.Notes = append(v.Notes, fmt.Sprintf("stage %d: step %d is not in the decoded plan (%d steps)", st.Number, s.Index, len(plan.Steps)))
					continue
				}
				if q := strings.TrimSpace(plan.Steps[s.Index].Query); q != "" && strings.TrimSpace(s.Query) != "" && q != strings.TrimSpace(s.Query) {
					v.Notes = append(v.Notes, fmt.Sprintf("stage %d step %d: recorded query differs from the decoded plan's", st.Number, s.Index))
				}
				end := s.EndNs
				if end < s.StartNs {
					end = s.StartNs
				}
				evs = append(evs, event{at: s.StartNs, s: s}, event{at: end, finish: true, s: s})
			}
			sort.SliceStable(evs, func(i, j int) bool {
				if evs[i].at != evs[j].at {
					return evs[i].at < evs[j].at
				}
				return !evs[i].finish && evs[j].finish
			})
			carriesOf := map[int][]domain.ToolEffect{}
			for _, e := range evs {
				s := e.s
				ps := plan.Steps[s.Index]
				var items []domain.RequirementItem
				for _, id := range ps.Answers {
					if it := cl2.Item(id); it != nil {
						items = append(items, *it)
					}
				}
				if !e.finish {
					// stepGuard (network/request_guards.go): only with request guards on.
					if guards {
						var writes []domain.RequirementItem
						for _, it := range items {
							if it.Type == domain.RequirementWrite && !cov.Status(it.ID).Final() {
								writes = append(writes, it)
							}
						}
						carriesOf[s.Index] = domain.CarriedClasses(writes, terms)
					}
					continue
				}
				actions, denials := planActions(s, tools, wt, carriesOf[s.Index])
				stageActions = append(stageActions, actions...)
				if len(items) == 0 || ps.IsThought {
					continue
				}
				obs := requirements.StepObservation{Step: s.Index, EvidenceKinds: map[string]domain.LedgerKind{}}
				ev := requirements.Evidence{Entries: map[string]domain.LedgerEntry{}}
				add := func(e domain.LedgerEntry) {
					if _, have := ev.Entries[e.ID]; have {
						return
					}
					ev.Entries[e.ID] = e
					obs.EvidenceKinds[e.ID] = e.Kind
				}
				for _, e := range byStep[s.Index] {
					add(e)
					if e.Kind == domain.LedgerToolResult {
						obs.ToolResults++
						if isErrorResult(e.Body) {
							obs.ToolErrors++
						}
					}
				}
				for _, dep := range depsOf(plan, s.Index) {
					for _, e := range byStep[dep] {
						add(e)
					}
					for _, ds := range g.Steps {
						if ds.Index == dep && ds.OutputRef != "" {
							if e, ok := requestEntries[ds.OutputRef]; ok {
								add(e)
							}
						}
					}
				}
				var body []byte
				if len(s.Outputs) > 0 {
					body = []byte(s.Outputs[len(s.Outputs)-1])
				}
				env, ok := domain.ParseAnswerEnvelope(body)
				if !ok {
					env = nil
				}
				if env != nil {
					obs.AnswerText = env.AnswerText
					// admitRequestEvidence: a cited entry of the same request (earlier
					// stages and generations, step outputs) is admitted.
					for _, id := range citedIDs(env) {
						if e, ok := requestEntries[id]; ok {
							add(e)
						}
					}
				}
				results, vs := requirements.ValidateEnvelope(env, items, ev)
				v.Envelopes = append(v.Envelopes, envelopeRows(st.Number, gi, s.Index, env, items, results, vs)...)
				obs.Results = results
				obs.Violations = vs
				obs.Member = ps.FanOutItem
				obs.SetMembers = fanOutDependencyMembers(plan, ps)
				obs.Actions, obs.Denials = actions, denials
				cov.ObserveStep(obs)
			}
			// This generation's entries join the request's (later stages may cite them).
			for _, es := range byStep {
				for _, e := range es {
					requestEntries[e.ID] = e
				}
			}
		}
		cov.ObserveActions(stageActions, effectsOf(tools))
		progress := cov.EndStage()
		row := stageRow{Number: st.Number, Progress: progress, Statuses: map[string]string{}}
		for _, it := range cov.Snapshot().Items {
			row.Statuses[it.Item.ID] = string(it.Status)
		}
		v.Stages = append(v.Stages, row)
	}
	if !cov.Settled() {
		cov.CloseGaps("replay: the recorded stages ended")
	}
	v.Coverage = cov.Snapshot()
	return v
}

// ── helpers: the reconstruction of kernel state the traces do not hold ──────

// scripted replays the recorded model replies in order; past the end it fails as
// a provider would, so the deriver takes its documented failure path.
type scripted struct {
	mu        sync.Mutex
	replies   []string
	n         int
	exhausted bool
}

func (s *scripted) Generate(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n >= len(s.replies) {
		s.exhausted = true
		return "", errors.New("judge replay: no recorded reply for this call")
	}
	r := s.replies[s.n]
	s.n++
	return r, nil
}

// captureHandler keeps the attributes of every log record by message.
type captureHandler struct {
	mu   sync.Mutex
	recs []capRec
}

type capRec struct {
	msg   string
	attrs map[string]string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.recs = append(h.recs, capRec{msg: r.Message, attrs: m})
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) reset() {
	h.mu.Lock()
	h.recs = nil
	h.mu.Unlock()
}
func (h *captureHandler) take(msg string) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.recs {
		if r.msg == msg {
			out = append(out, r.attrs)
		}
	}
	return out
}

// invalidTypeDrops lists reply items whose type the deriver drops outright.
func invalidTypeDrops(replies []string) []map[string]string {
	var out []map[string]string
	for i, r := range replies {
		var m struct {
			Items []struct {
				Type, Text, Quote string
			} `json:"items"`
		}
		if json.Unmarshal([]byte(stripFence(r)), &m) != nil {
			continue
		}
		for _, it := range m.Items {
			if !domain.ValidRequirementType(domain.RequirementType(strings.ToLower(strings.TrimSpace(it.Type)))) {
				out = append(out, map[string]string{"call": fmt.Sprint(i), "type": it.Type, "text": it.Text, "quote": it.Quote})
			}
		}
	}
	return out
}

func stripFence(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	return text
}

// decodePlan decodes a planner reply into the plan (the reply is the plan plus
// reasoning/outcome fields) and applies the planner's deterministic repair.
func decodePlan(reply string) (*domain.ExecutionPlan, error) {
	var p domain.ExecutionPlan
	if err := json.Unmarshal([]byte(stripFence(reply)), &p); err != nil {
		return nil, err
	}
	domain.RepairSelfDependencies(&p)
	return &p, nil
}

func validatePlan(stage, call int, pc planCall, cl *domain.RequirementChecklist) planRow {
	row := planRow{Stage: stage, Call: call, Recorded: pc.Recorded, RecordedRule: pc.RecordedRule}
	plan, err := decodePlan(pc.Reply)
	if err != nil {
		row.DecodeError = err.Error()
		row.Replayed = "undecodable"
		return row
	}
	if len(plan.Steps) == 0 {
		row.Replayed = "no_steps" // read / input_required / nothing_more replies
		return row
	}
	vocab, _ := config.CapabilityConfig{}.StepEffectVocabulary()
	vs := planvalidate.Validate(planvalidate.Input{Plan: plan, Checklist: cl, Vocabulary: vocab,
		DateSources: []string{pc.Input}, Staged: pc.Staged, Settled: pc.Settled, Earlier: pc.Earlier})
	row.Violations = vs
	row.Replayed = "accepted"
	if len(vs) > 0 {
		row.Replayed = "rejected"
	}
	return row
}

// toolPolicy is one tool's ADR-0086 classes from the run's mcp.json.
type toolPolicy struct {
	dangerous bool
	effects   []domain.ToolEffect
}

type toolPolicies struct {
	byTool  map[string]toolPolicy  // "mcp:<server>/<tool>"
	servers map[string]*toolPolicy // server default; nil value = fail closed
}

// loadToolPolicies reads the mcp.json tool policies (ADR-0139 D8: a tool with no
// entry takes the server's default_tool_policy, else fails closed as dangerous
// read+write+egress).
func loadToolPolicies(path string) (*toolPolicies, error) {
	tp := &toolPolicies{byTool: map[string]toolPolicy{}, servers: map[string]*toolPolicy{}}
	if path == "" {
		return tp, errors.New("no mcp.json given: every MCP tool fails closed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return tp, err
	}
	var doc struct {
		MCP config.MCPConfig `json:"mcp"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return tp, err
	}
	conv := func(tc config.MCPToolConfig) toolPolicy {
		p := toolPolicy{dangerous: tc.Dangerous}
		for _, e := range tc.Effects {
			p.effects = append(p.effects, domain.ToolEffect(strings.ToLower(e)))
		}
		return p
	}
	for _, s := range doc.MCP.Servers {
		for _, tc := range s.Tools {
			tp.byTool["mcp:"+s.ID+"/"+tc.Name] = conv(tc)
		}
		if s.DefaultToolPolicy != nil {
			p := conv(*s.DefaultToolPolicy)
			tp.servers[s.ID] = &p
		} else {
			tp.servers[s.ID] = nil
		}
	}
	return tp, nil
}

var failClosed = toolPolicy{dangerous: true, effects: []domain.ToolEffect{domain.EffectRead, domain.EffectWrite, domain.EffectEgress}}

// policy returns a tool's classes and whether the registry knows it. Kernel tools
// (ledger_get, calc_*) are read-only.
func (tp *toolPolicies) policy(tool string) (toolPolicy, bool) {
	if !strings.HasPrefix(tool, "mcp:") {
		return toolPolicy{effects: []domain.ToolEffect{domain.EffectRead}}, true
	}
	if p, ok := tp.byTool[tool]; ok {
		return p, true
	}
	server := strings.TrimPrefix(tool, "mcp:")
	if i := strings.IndexByte(server, '/'); i >= 0 {
		server = server[:i]
	}
	if d, ok := tp.servers[server]; ok {
		if d != nil {
			return *d, true
		}
		return failClosed, true
	}
	return failClosed, false
}

// sideEffecting mirrors network.sideEffectingTool: dangerous, or an effect other
// than read/egress; an unknown tool counts as effecting.
func (tp *toolPolicies) sideEffecting(tool string) (known, effecting bool) {
	p, known := tp.policy(tool)
	if !known {
		return false, true
	}
	if p.dangerous {
		return true, true
	}
	for _, e := range p.effects {
		if e != domain.EffectRead && e != domain.EffectEgress {
			return true, true
		}
	}
	return true, false
}

// planActions mirrors network.recordPlanAction for one step's recorded calls.
func planActions(s caseStep, tp *toolPolicies, wt *domain.WriteTargets, carries []domain.ToolEffect) (actions, denials []domain.PlanAction) {
	for _, c := range s.Calls {
		known, effecting := tp.sideEffecting(c.Tool)
		args := redacted(c.Args)
		if !effecting {
			if c.Denied && known {
				denials = append(denials, domain.PlanAction{Step: s.Index, Tool: c.Tool, Args: args,
					Status: domain.PlanActionRefused, Detail: c.DenyReason, ApprovalState: domain.ApprovalState(c.ApprovalState)})
			}
			continue
		}
		a := domain.PlanAction{Step: s.Index, Tool: c.Tool, Args: args, Target: wt.Key(c.Tool, []byte(c.Args)),
			Carries: append([]domain.ToolEffect(nil), carries...)}
		switch {
		case c.Denied:
			if !known {
				continue
			}
			a.Status, a.Detail, a.ApprovalState = domain.PlanActionRefused, c.DenyReason, domain.ApprovalState(c.ApprovalState)
		case c.Error != "":
			a.Status, a.Detail = domain.PlanActionFailed, c.Error
		default:
			a.Status = domain.PlanActionDone
			a.IDs = domain.PlanActionIDs([]byte(c.Result))
		}
		actions = append(actions, a)
	}
	return actions, denials
}

func redacted(argsJSON string) map[string]string {
	var m map[string]any
	if strings.TrimSpace(argsJSON) == "" || json.Unmarshal([]byte(argsJSON), &m) != nil {
		return nil
	}
	return domain.RedactArgs(m)
}

// effectsOf mirrors network.effectsOfTool (ADR-0145 D14.6 Carries excluded).
func effectsOf(tp *toolPolicies) func(domain.PlanAction) []string {
	terms, err := config.ToolsConfig{}.ConstraintTerms()
	if err != nil {
		return nil
	}
	return func(a domain.PlanAction) []string {
		p, _ := tp.policy(a.Tool)
		t := domain.SystemTool{Name: a.Tool, Effects: p.effects}
		var out []string
		for term, classes := range terms {
			for _, c := range classes {
				if t.HasEffect(c) && !containsEffect(a.Carries, c) {
					out = append(out, term)
					break
				}
			}
		}
		sort.Strings(out)
		return out
	}
}

func containsEffect(xs []domain.ToolEffect, x domain.ToolEffect) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// entryOf is the ledger entry a recorded call left (only calls with a ledger id).
func entryOf(c toolCall, step int) (domain.LedgerEntry, bool) {
	if !domain.IsLedgerID(c.LedgerID) {
		return domain.LedgerEntry{}, false
	}
	kind := domain.LedgerToolResult
	if strings.HasPrefix(c.Tool, "calc_") {
		kind = domain.LedgerComputed
	}
	body := c.Result
	if c.Error != "" {
		b, _ := json.Marshal(map[string]string{"error": c.Error})
		body = string(b)
	}
	return domain.LedgerEntry{ID: c.LedgerID, Kind: kind, StepIndex: step, ToolName: c.Tool, Body: []byte(body),
		Truncated: c.BodyMissing}, true
}

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

func citedIDs(env *domain.AnswerEnvelope) []string {
	var out []string
	for _, r := range env.ItemResults {
		for _, v := range r.Values {
			out = append(out, strings.TrimSpace(v.EvidenceRef))
			if v.Type == domain.ValueRef {
				out = append(out, strings.TrimSpace(v.Text()))
			}
		}
	}
	return append(out, env.EvidenceRefs...)
}

func depsOf(plan *domain.ExecutionPlan, step int) []int {
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

func fanOutDependencyMembers(plan *domain.ExecutionPlan, st domain.Step) []string {
	if st.FanOutItem != "" {
		return nil
	}
	var out []string
	for _, dep := range st.DependsOn {
		if dep >= 0 && dep < len(plan.Steps) {
			if m := plan.Steps[dep].FanOutItem; m != "" && !containsStr(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func envelopeRows(stage, gen, step int, env *domain.AnswerEnvelope, items []domain.RequirementItem,
	results []domain.ItemResult, vs []domain.EnvelopeViolation) []envelopeRow {
	types := map[string]string{}
	for _, it := range items {
		types[it.ID] = string(it.Type)
	}
	if env == nil {
		var out []envelopeRow
		for _, it := range items {
			if it.Type == domain.RequirementConstraint {
				continue
			}
			out = append(out, envelopeRow{Stage: stage, Generation: gen, Step: step, Item: it.ID, ItemType: string(it.Type), NoEnvelope: true, Violations: vs})
		}
		return out
	}
	kept := map[string]int{}
	for _, r := range results {
		kept[r.Item] += len(r.Values)
	}
	var out []envelopeRow
	for _, r := range env.ItemResults {
		row := envelopeRow{Stage: stage, Generation: gen, Step: step, Item: r.Item, ItemType: types[r.Item],
			Claimed: string(r.Status), Sent: len(r.Values), Kept: kept[r.Item]}
		for _, x := range vs {
			if x.Item == r.Item {
				row.Violations = append(row.Violations, x)
			}
		}
		out = append(out, row)
	}
	return out
}

// acceptsWrite asks the real coverage judge whether a done write of path settles
// item it: one step that claims the item satisfied with one done write_file to the
// path (its write record target from the shipped WriteTargets rules).
func acceptsWrite(it domain.RequirementItem, path string, wt *domain.WriteTargets) bool {
	it.ID = "R1"
	cov := requirements.NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{it}})
	args, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
	tool := "mcp:filesystem/write_file"
	cov.ObserveStep(requirements.StepObservation{Step: 0,
		Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{{Step: 0, Tool: tool, Status: domain.PlanActionDone, Target: wt.Key(tool, args),
			Args: map[string]string{"path": path}}}})
	return cov.Status("R1") == domain.RequirementSatisfied
}
