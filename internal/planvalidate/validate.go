// Package planvalidate is the ADR-0145 D7 plan validator: the checks a plan must
// pass, in code, before it runs (execution.plan.plan_validator;
// T-20261108-adr-0145-phase-2-requirement-checklist-with). It replaces the
// DAG-only check (network.validatePlan) when the flag is on. A rejected plan goes
// back to the planner with the structured violations; nothing here repairs a
// plan by itself.
//
// Rule groups (D7):
//   - structure: a DAG, dependencies in range, every input reference
//     (domain.InputRef: s<N> or e<K>) names a step the step depends on or an
//     earlier stage's result (Input.Earlier) — a rejected one is answered with
//     the valid references and the closest — a fan-out names an earlier step and is templated (it
//     binds to a list), for_each names a dependency or the fan-out source and the
//     step fans out over it (coverage per member is per fan-out child);
//   - coverage (with a checklist): every answers id is a checklist item and every
//     item but a constraint has a producing step;
//   - written targets (ADR-0145 D14): a step that writes (it answers a write or
//     document item, or declares an effect that is not read-only) may not name a
//     target the request already wrote — a target is written once per request,
//     across stages, whatever the step's answers labels say;
//   - dates: every date literal in a query is a date the kernel's calendar block,
//     the user's request or an earlier result states;
//   - decomposition (D13, with a checklist): a judgement is one step that sees
//     the whole picture (decomposition below).
//
// There are no effect rules: ADR-0145 D14 removed per-step permissions. The
// person's "don't" constraints are enforced at call time by tool class, and a
// repeated write by the tool executor's per-target write record; this validator
// only tells the planner early about targets already written.
package planvalidate

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// Violation is one rule a plan broke, structured for the planner's repair turn.
type Violation struct {
	Step   int    `json:"step"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// Rules.
const (
	RuleCycle         = "structure.cycle"
	RuleDependency    = "structure.dependency"
	RuleBinding       = "structure.binding"
	RuleFanOut        = "structure.fan_out"
	RuleForEach       = "structure.for_each"
	RuleUnknownItem   = "coverage.unknown_item"
	RuleUncovered     = "coverage.uncovered_item"
	RuleWrittenTarget = "coverage.written_target"
	RuleDate          = "dates.unsourced_date"
	// The staged planner's coverage rules (ADR-0145 D11): a stage answers open
	// items only, and at least one of them.
	RuleSettledItem = "coverage.settled_item"
	RuleNoOpenItem  = "coverage.no_open_item"
	// The D13 decomposition rules: a judgement is made in one step that sees
	// everything relevant, never spread across steps that each see part of it.
	RuleSplitJudgement   = "decomposition.split_judgement"
	RuleFanOutJudgement  = "decomposition.fan_out_judgement"
	RulePartialSetInputs = "decomposition.partial_set_inputs"
)

// Input is what the validator checks a plan against.
type Input struct {
	Plan *domain.ExecutionPlan
	// Checklist enables the coverage rules; nil skips them.
	Checklist *domain.RequirementChecklist
	// Vocabulary tells a step that declares a non-read effect as a writing step
	// for the written-target rule; nil: only write/document answers count.
	Vocabulary *domain.StepEffectVocabulary
	// Written are the targets the request already wrote (the tool executor's
	// write record, ADR-0145 D14).
	Written []domain.WrittenTarget
	// DateSources are texts whose dates a query may use: the calendar block, the
	// user's request, earlier stages' results.
	DateSources []string
	// Staged switches the coverage rules to one stage of the staged planner
	// (ADR-0145 D11): the plan is the NEXT stage, so it need not answer every item;
	// it must answer at least one open item and no settled one (Settled). Off: the
	// plan is the whole work and every item needs a producing step.
	Staged  bool
	Settled []string
	// Earlier are the earlier stages' results a step may bind as e<K>
	// (domain.InputRef); nil on the first stage and outside the coverage stages.
	Earlier []domain.EarlierResult
}

// Validate returns every violation (nil for a valid plan).
func Validate(in Input) []Violation {
	p := in.Plan
	if p == nil {
		return []Violation{{Step: -1, Rule: RuleDependency, Detail: "no plan"}}
	}
	var vs []Violation
	add := func(step int, rule, f string, a ...any) {
		vs = append(vs, Violation{Step: step, Rule: rule, Detail: fmt.Sprintf(f, a...)})
	}
	n := len(p.Steps)
	if _, err := executer.TopologicalSort(p.Steps); err != nil {
		var cyc *executer.CyclicPlanError
		if errors.As(err, &cyc) {
			add(-1, RuleCycle, "the plan contains a dependency cycle: %s", cyc.Description)
		} else {
			add(-1, RuleDependency, "invalid dependency structure: %v", err)
		}
	}
	for i, st := range p.Steps {
		for _, d := range st.DependsOn {
			if d < 0 || d >= n || d == i {
				add(i, RuleDependency, "depends_on %d is not another step of the plan (steps are 0..%d)", d, n-1)
			}
		}
		for _, b := range st.Inputs {
			ref, ok := domain.ParseInputRef(b)
			switch {
			case !ok:
				add(i, RuleBinding, "input %q is not a reference%s", b, refHelp(b, i, n, in.Earlier))
			case ref.Earlier && earlierByLabel(in.Earlier, ref.Source()) == nil:
				add(i, RuleBinding, "input %q names %s, which is not an earlier result of this request%s", b, ref.Source(), refHelp(b, i, n, in.Earlier))
			case ref.Earlier:
				// An earlier stage's result has finished: it is bound, never a dependency.
			case ref.N < 0 || ref.N >= n || ref.N == i:
				add(i, RuleBinding, "input %q names step %d, which is not another step of this plan%s", b, ref.N, refHelp(b, i, n, in.Earlier))
			case !containsInt(st.DependsOn, ref.N) && !fanSource(st, ref.N):
				add(i, RuleBinding, "input %q binds step %d, which is not in depends_on: add %d to depends_on", b, ref.N, ref.N)
			}
		}
		if st.FanOutOver != nil {
			src := *st.FanOutOver
			if src < 0 || src >= n || src == i {
				add(i, RuleFanOut, "fan_out_over %d is not another step of the plan", src)
			}
			if !st.FanOutTemplated() {
				add(i, RuleFanOut, "a fan-out step's query must describe one item and contain {%s}", st.FanOutVarName())
			}
		}
		if st.ForEach != "" {
			src, _, _, ok := domain.ParseBinding(st.ForEach)
			switch {
			case !ok:
				add(i, RuleForEach, "for_each %q is not a binding (s<N>.<name>)", st.ForEach)
			case !containsInt(st.DependsOn, src) && !fanSource(st, src):
				add(i, RuleForEach, "for_each %q names step %d, which is neither in depends_on nor the fan-out source", st.ForEach, src)
			case st.FanOutOver == nil:
				// Coverage per member is enforced through the fan-out's children
				// (one result each); a for_each step that does not fan out cannot
				// say which member a result is for.
				add(i, RuleForEach, "for_each %q needs fan_out_over %d, so each member gets its own step and result", st.ForEach, src)
			}
		}
	}
	if in.Checklist != nil {
		vs = append(vs, coverage(p, in.Checklist, in.Staged, in.Settled)...)
	}
	if len(in.Written) > 0 {
		vs = append(vs, writtenTargets(p, in.Checklist, in.Vocabulary, in.Written)...)
	}
	vs = append(vs, dates(p, in.DateSources)...)
	if in.Checklist != nil {
		vs = append(vs, decomposition(p, in.Checklist)...)
	}
	return vs
}

func coverage(p *domain.ExecutionPlan, c *domain.RequirementChecklist, staged bool, settled []string) []Violation {
	var vs []Violation
	produced := map[string]bool{}
	for i, st := range p.Steps {
		for _, id := range st.Answers {
			it := c.Item(id)
			if it == nil {
				vs = append(vs, Violation{Step: i, Rule: RuleUnknownItem,
					Detail: fmt.Sprintf("answers names %q, which is not a checklist item", id)})
				continue
			}
			if staged && containsStr(settled, id) {
				vs = append(vs, Violation{Step: i, Rule: RuleSettledItem,
					Detail: fmt.Sprintf("answers names %s, which is already settled: plan only open items", id)})
				continue
			}
			produced[id] = true
		}
	}
	if staged {
		// One stage: progress needs at least one open item answered; the rest can
		// wait for a later stage that sees this one's results.
		for _, it := range c.Items {
			if it.Type != domain.RequirementConstraint && produced[it.ID] {
				return vs
			}
		}
		return append(vs, Violation{Step: -1, Rule: RuleNoOpenItem,
			Detail: "no step answers an open item: every stage answers at least one (reply nothing_more when nothing can)"})
	}
	for _, it := range c.Items {
		if it.Type != domain.RequirementConstraint && !produced[it.ID] {
			vs = append(vs, Violation{Step: -1, Rule: RuleUncovered, Detail: fmt.Sprintf(
				"no step answers %s (%s: %s)", it.ID, it.Type, it.Text)})
		}
	}
	return vs
}

// writtenTargets refuses a writing step that names a target the request already
// wrote (its value appears in the step's query, case-insensitively).
func writtenTargets(p *domain.ExecutionPlan, c *domain.RequirementChecklist, v *domain.StepEffectVocabulary, written []domain.WrittenTarget) []Violation {
	writes := func(st domain.Step) bool {
		for _, id := range st.Answers {
			// ADR-0145 D14.6: only a write item puts anything into an outside system
			// (a document is finished inside Cambrian).
			if it := c.Item(id); it != nil && it.Type == domain.RequirementWrite {
				return true
			}
		}
		for _, e := range st.RequiredEffects {
			if v != nil && v.Known(e) && !v.ReadOnly(e) {
				return true
			}
		}
		return false
	}
	var vs []Violation
	for i, st := range p.Steps {
		if !writes(st) {
			continue
		}
		q := strings.ToLower(st.Query + " " + st.FanOutItem)
		for _, w := range written {
			for _, val := range strings.Split(domain.TargetValue(w.Target), ",") {
				val = strings.TrimSpace(val)
				if len(val) < 3 || !strings.Contains(q, val) {
					continue
				}
				ids := ""
				if len(w.IDs) > 0 {
					ids = " (" + strings.Join(w.IDs, ", ") + ")"
				}
				vs = append(vs, Violation{Step: i, Rule: RuleWrittenTarget, Detail: fmt.Sprintf(
					"this step writes to %s, which this request already wrote with %s%s: a target is written once per request; do not plan another write to it, report the earlier result",
					val, w.Tool, ids)})
			}
		}
	}
	return vs
}

var (
	isoDate    = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	dottedDate = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)
)

// dateKeys extracts every calendar date a text states, as YYYY-MM-DD.
func dateKeys(text string) []string {
	var out []string
	for _, m := range isoDate.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1]+"-"+m[2]+"-"+m[3])
	}
	for _, m := range dottedDate.FindAllStringSubmatch(text, -1) {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		out = append(out, fmt.Sprintf("%s-%02d-%02d", m[3], mo, d))
	}
	return out
}

func dates(p *domain.ExecutionPlan, sources []string) []Violation {
	known := map[string]bool{}
	for _, s := range sources {
		for _, k := range dateKeys(s) {
			known[k] = true
		}
	}
	var vs []Violation
	for i, st := range p.Steps {
		for _, k := range dateKeys(st.Query) {
			if !known[k] {
				vs = append(vs, Violation{Step: i, Rule: RuleDate, Detail: fmt.Sprintf(
					"the query states %s, which neither <today>, the request nor an earlier result gives: copy dates, never compute them", k)})
			}
		}
	}
	return vs
}

func fanSource(st domain.Step, src int) bool { return st.FanOutOver != nil && *st.FanOutOver == src }

func earlierByLabel(es []domain.EarlierResult, label string) *domain.EarlierResult {
	for i := range es {
		if es[i].Label == label {
			return &es[i]
		}
	}
	return nil
}

// refHelp is the repair half of a rejected input reference
// (T-20261112-staged-planner-binds-earlier-stage-results-with; research note
// 2026-11-12 §7): the grammar, every reference valid for step i of an n-step plan,
// and the closest valid one. The closest is a suggestion for the planner's next
// attempt; the plan is never rewritten with it.
func refHelp(bad string, i, n int, earlier []domain.EarlierResult) string {
	var own []string
	for k := 0; k < n; k++ {
		if k != i {
			own = append(own, fmt.Sprintf("s%d", k))
		}
	}
	var b strings.Builder
	b.WriteString(". A reference is s<N> (step N of this plan, also in depends_on) or e<K> (an earlier stage's result as labelled in <stage>), optionally followed by .<name> and [*]. Valid here: ")
	switch {
	case len(own) == 0 && len(earlier) == 0:
		b.WriteString("none (this plan has no other step and no earlier stage ran)")
	default:
		var all []string
		all = append(all, own...)
		for _, e := range earlier {
			all = append(all, fmt.Sprintf("%s (stage %d step %d)", e.Label, e.Stage, e.Step))
		}
		b.WriteString(strings.Join(all, ", "))
	}
	if c := closestRef(bad, own, earlier); c != "" {
		fmt.Fprintf(&b, ". Closest: %s", c)
	}
	return b.String()
}

var refNumberRe = regexp.MustCompile(`\d+`)

// closestRef suggests the valid reference a rejected one most likely meant: the
// earlier result whose ledger id it names; the earlier result of the stage and
// step its two numbers name ("s1.step_3", "stage1.s3" -> stage 1 step 3); else the
// valid source at the smallest edit distance from its source part. "" when there
// is no valid reference.
func closestRef(bad string, own []string, earlier []domain.EarlierResult) string {
	t := strings.ToLower(strings.TrimSpace(bad))
	for _, e := range earlier {
		if e.Ref != "" && strings.Contains(t, strings.ToLower(e.Ref)) {
			return e.Label
		}
	}
	if nums := refNumberRe.FindAllString(t, -1); len(nums) == 2 {
		st, _ := strconv.Atoi(nums[0])
		sp, _ := strconv.Atoi(nums[1])
		for _, e := range earlier {
			if e.Stage == st && e.Step == sp {
				return e.Label
			}
		}
	}
	src, _, _ := strings.Cut(strings.TrimSuffix(t, "[*]"), ".")
	best, bestD := "", -1
	cands := append([]string(nil), own...)
	for _, e := range earlier {
		cands = append(cands, e.Label)
	}
	for _, c := range cands {
		if d := editDistance(src, c); bestD < 0 || d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Render formats violations for the planner's repair turn: one JSON object per
// line, so the model reads each rule, step and detail separately.
func Render(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "{\"step\": %d, \"rule\": %q, \"detail\": %q}\n", v.Step, v.Rule, v.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// decomposition is the ADR-0145 D13 rule as plan checks
// (T-20261108-plan-validator-enforce-the-d13-decomposition):
//   - J1 split judgement: an item no step ties to a set (no for_each) answered by
//     more than one step, where no answering step depends (transitively) on all
//     the others: the answer would be decided by steps that each saw part of it;
//   - J2 fan-out judgement: a fan-out step (one child per member) answering an
//     item without for_each decides a set-level item from one member;
//   - J3 partial set inputs: when an item is tied to a set (some answering step
//     has for_each sK.x), every other step answering it must see the set — depend
//     on sK (transitively) or fan out over it.
//
// Constraints are never answered, and a fan-out's concrete children (FanOutItem)
// are one producer, as in the effect rules.
func decomposition(p *domain.ExecutionPlan, c *domain.RequirementChecklist) []Violation {
	var vs []Violation
	answering := map[string][]int{}
	setOf := map[string]int{} // item -> the set's source step, when tied to one
	for i, st := range p.Steps {
		if st.FanOutItem != "" {
			continue
		}
		for _, id := range st.Answers {
			it := c.Item(id)
			if it == nil || it.Type == domain.RequirementConstraint {
				continue
			}
			answering[id] = append(answering[id], i)
			if st.ForEach != "" {
				if src, _, _, ok := domain.ParseBinding(st.ForEach); ok {
					setOf[id] = src
				}
			} else if st.FanOutOver != nil {
				vs = append(vs, Violation{Step: i, Rule: RuleFanOutJudgement, Detail: fmt.Sprintf(
					"a fan-out step answers %s for one member at a time; set for_each when %s is per member, or answer it in one step that depends on this one and sees every member", id, id)})
			}
		}
	}
	for _, it := range c.Items {
		steps := answering[it.ID]
		if len(steps) < 2 {
			continue
		}
		if src, tied := setOf[it.ID]; tied {
			for _, i := range steps {
				if fanSource(p.Steps[i], src) || dependsOn(p, i, src) {
					continue
				}
				vs = append(vs, Violation{Step: i, Rule: RulePartialSetInputs, Detail: fmt.Sprintf(
					"%s is answered per member of the set step %d finds; this step answers it without seeing that set: depend on step %d", it.ID, src, src)})
			}
			continue
		}
		joined := false
		for _, i := range steps {
			all := true
			for _, j := range steps {
				if j != i && !dependsOn(p, i, j) {
					all = false
					break
				}
			}
			if all {
				joined = true
				break
			}
		}
		if !joined {
			vs = append(vs, Violation{Step: steps[1], Rule: RuleSplitJudgement, Detail: fmt.Sprintf(
				"steps %v each answer %s with part of the picture: answer %s in one step that depends on the others", steps, it.ID, it.ID)})
		}
	}
	return vs
}

// dependsOn reports whether step i depends, transitively, on step j (through
// depends_on or a fan-out source).
func dependsOn(p *domain.ExecutionPlan, i, j int) bool {
	seen := map[int]bool{}
	stack := append([]int(nil), p.Steps[i].DependsOn...)
	if p.Steps[i].FanOutOver != nil {
		stack = append(stack, *p.Steps[i].FanOutOver)
	}
	for len(stack) > 0 {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if k < 0 || k >= len(p.Steps) || seen[k] {
			continue
		}
		if k == j {
			return true
		}
		seen[k] = true
		stack = append(stack, p.Steps[k].DependsOn...)
		if p.Steps[k].FanOutOver != nil {
			stack = append(stack, *p.Steps[k].FanOutOver)
		}
	}
	return false
}
