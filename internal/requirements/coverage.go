package requirements

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// Coverage is the item state machine of one delegated request (ADR-0145 D5).
// Statuses change only here, from what the kernel itself recorded: validated
// envelope values, the ledger's tool results for the step, and the plan's action
// ledger. A worker's status is a claim this type accepts or refuses by the rules
// below; the planner never sets one. Safe for concurrent use (steps of one stage
// finish in parallel).
//
// Per type (D5.3):
//   - answer: satisfied by at least one validated value of a type the item
//     expects (RequirementItem.Expects; none declared: any type);
//   - figure: the same, with a number, money amount or date when it declares no
//     expects (the validator already refused a figure value that cites model text);
//   - document: by a value citing an artifact entry, or a done side-effecting call
//     of the step (the draft or file exists in the action ledger);
//   - write: by a done side-effecting call of the step to one of the item's
//     declared targets (targetActions; none declared: any done write of the step);
//     another tool succeeding never does;
//   - constraint: never satisfied; a done or attempted call whose effects the
//     constraint forbids fails it (ObserveActions).
//
// not_found is accepted only when the step's tool results show real searches
// (D5.4); when every search errored the item stays open with a retry instruction,
// and is accepted only after a second attempt. not_permitted is accepted only when
// the kernel recorded a refusal for the step. Two different values for the same
// item and name keep it open as a conflict (D5.7). An item tied to a set
// (for_each / fan-out) needs one accepted result per member (D5.6).
type Coverage struct {
	mu    sync.Mutex
	items []*ItemState
	byID  map[string]*ItemState
	// lastPrint is the progress fingerprint at the end of the previous stage.
	lastPrint string
	stages    int
	stalled   bool
}

// ItemState is one item's state.
type ItemState struct {
	Item   domain.RequirementItem   `json:"item"`
	Status domain.RequirementStatus `json:"status"`
	Reason string                   `json:"reason,omitempty"`
	Values []ItemValue              `json:"values,omitempty"`
	// Evidence are the ledger ids behind the accepted values.
	Evidence []string `json:"evidence,omitempty"`
	// Members are the for_each members expected; Covered those with an accepted
	// result. Empty Members: not a set item.
	Members []string        `json:"members,omitempty"`
	Covered map[string]bool `json:"covered,omitempty"`
	// Retry is the instruction for the next stage when the earlier attempt must not
	// be repeated as it was.
	Retry string `json:"retry,omitempty"`
	// Conflict names two disagreeing values; the item stays open while it is set.
	Conflict string `json:"conflict,omitempty"`
	// Steps are the plan steps that reported on the item.
	Steps []int `json:"steps,omitempty"`
	// Targets are the per-target outcomes of the side-effecting calls of the steps
	// that answered a write or document item (ADR-0145 D14): a refusal of one
	// target never erases another target's executed write.
	Targets []TargetResult `json:"targets,omitempty"`
	// erroredSearches counts the stages whose not_found rested only on errored
	// searches (D5.4: a second attempt is required first).
	erroredSearches int
}

// TargetResult is one write target's outcome for an item.
type TargetResult struct {
	Target string                   `json:"target"`
	Tool   string                   `json:"tool"`
	Status domain.RequirementStatus `json:"status"`
	Step   int                      `json:"step"`
	IDs    []string                 `json:"ids,omitempty"`
	Detail string                   `json:"detail,omitempty"`
}

// ItemValue is an accepted value with where it came from.
type ItemValue struct {
	domain.EnvelopeValue
	Step   int    `json:"step"`
	Member string `json:"member,omitempty"`
	// Refused is set while the judge has not accepted a result carrying the value
	// (a type the item does not expect, a refused claim). It is kept for the
	// report but is not progress (EndStage).
	Refused bool `json:"refused,omitempty"`
}

// NewCoverage starts every item of c open.
func NewCoverage(c domain.RequirementChecklist) *Coverage {
	cov := &Coverage{byID: map[string]*ItemState{}}
	for _, it := range c.Items {
		st := &ItemState{Item: it, Status: domain.RequirementOpen}
		cov.items = append(cov.items, st)
		cov.byID[it.ID] = st
	}
	cov.lastPrint = cov.fingerprint()
	return cov
}

// StepObservation is what the kernel knows about one finished step.
type StepObservation struct {
	Step int
	// Member is the fan-out item the step ran for ("" for an ordinary step).
	Member string
	// Results are the step's validated item results (ValidateEnvelope's output).
	Results []domain.ItemResult
	// Violations left after the repair turn, per item, become the open reason.
	Violations []domain.EnvelopeViolation
	// ToolResults counts the step's tool_result ledger entries; ToolErrors those
	// that recorded an error.
	ToolResults int
	ToolErrors  int
	// EvidenceKinds maps each cited ledger id to its kind.
	EvidenceKinds map[string]domain.LedgerKind
	// Actions are the step's side-effecting calls from the plan's action ledger.
	Actions []domain.PlanAction
	// Denials are the step's refused READ calls (the kernel's record that access
	// was refused: scope, grant or a denied approval on a read). With Actions they
	// are what not_permitted is accepted on
	// (T-20261108-coverage-cannot-accept-not-permitted-for-a).
	Denials []domain.PlanAction
	// AnswerText is the step's envelope answer_text. For a document item it is the
	// draft itself: the step output is recorded in the run ledger by the executor,
	// so a document is finished inside Cambrian (ADR-0145 D14.6).
	AnswerText string
	// SetMembers are the fan-out members the step consumed (the FanOutItem of each
	// fan-out child it depends on). A write item answered by one ordinary step over
	// that set needs a done write per member (coverMembersByWrites).
	SetMembers []string
}

// ExpectMembers ties an item to a set: one accepted result per member is required.
// Called when a fan-out step answering the item expands. Members accumulate across
// expansions and stages; a member already covered stays covered.
func (c *Coverage) ExpectMembers(itemID string, members []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.byID[itemID]
	if st == nil {
		return
	}
	for _, m := range members {
		if m != "" && !containsStr(st.Members, m) {
			st.Members = append(st.Members, m)
		}
	}
	if len(st.Members) > 0 && st.Status == domain.RequirementSatisfied && len(st.missing()) > 0 {
		st.Status = domain.RequirementOpen // a set grew under a satisfied item
		st.Reason = "members with no result: " + strings.Join(st.missing(), ", ")
	}
}

// ObserveStep applies one step's validated results.
func (c *Coverage) ObserveStep(o StepObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reasons := map[string][]string{}
	for _, v := range o.Violations {
		if v.Item != "" {
			reasons[v.Item] = append(reasons[v.Item], v.Detail)
		}
	}
	for _, r := range o.Results {
		st := c.byID[r.Item]
		if st == nil || st.Item.Type == domain.RequirementConstraint {
			continue
		}
		st.Steps = appendUniqueInt(st.Steps, o.Step)
		c.applyResult(st, o, r, reasons[r.Item])
	}
	// An item with only violations (no result survived) keeps the violation as its
	// reason so the next stage knows why.
	for item, rs := range reasons {
		if st := c.byID[item]; st != nil && !st.Status.Final() && st.Reason == "" {
			st.Reason = "the step's answer was not accepted: " + strings.Join(rs, "; ")
		}
	}
}

func (c *Coverage) applyResult(st *ItemState, o StepObservation, r domain.ItemResult, violations []string) {
	// Values first: they are kept with their evidence whatever the claim, and a
	// disagreement with an earlier accepted value is a conflict. A value counts as
	// progress (fingerprint) only once the judge accepted the result it came with:
	// a worker re-sending a refused value under a new name each stage is a stall,
	// not progress (T-20261112-checklist-types-the-source-page-for-each: p03 ran to
	// stage 7 re-reporting the same refused URLs).
	var added []int
	for _, v := range r.Values {
		if prev, ok := st.conflictWith(v, o); ok {
			st.Conflict = fmt.Sprintf("%s is %q (step %d) and %q (step %d)", v.Name, prev.Text(), prev.Step, v.Text(), o.Step)
			continue
		}
		i := st.valueIndex(v, o)
		if i < 0 {
			st.Values = append(st.Values, ItemValue{EnvelopeValue: v, Step: o.Step, Member: o.Member, Refused: true})
			i = len(st.Values) - 1
		}
		added = append(added, i)
		st.Evidence = appendUniqueStr(st.Evidence, v.EvidenceRef)
	}
	if st.Conflict != "" {
		st.Status = domain.RequirementOpen
		st.Reason = "conflict: " + st.Conflict + "; resolve which value is right"
		return
	}
	if st.Item.Type == domain.RequirementWrite || st.Item.Type == domain.RequirementDocument {
		st.observeTargets(o)
	}
	accepted, status, reason := c.judge(st, o, r)
	if accepted {
		for _, i := range added {
			st.Values[i].Refused = false
		}
	}
	if len(violations) > 0 && !accepted {
		reason = strings.TrimSpace(reason + " (" + strings.Join(violations, "; ") + ")")
	}
	if !accepted {
		if !st.Status.Final() {
			st.Reason = reason
		}
		return
	}
	if st.Item.Type == domain.RequirementWrite && o.Member == "" {
		st.coverMembersByWrites(o)
	}
	if len(st.Members) > 0 || o.Member != "" {
		if o.Member != "" {
			if st.Covered == nil {
				st.Covered = map[string]bool{}
			}
			st.Covered[o.Member] = true
			if !containsStr(st.Members, o.Member) {
				st.Members = append(st.Members, o.Member)
			}
		}
		if miss := st.missing(); len(miss) > 0 {
			st.Status = domain.RequirementOpen
			st.Reason = "members with no result: " + strings.Join(miss, ", ")
			return
		}
		st.Status = domain.RequirementSatisfied
		st.Reason = reason
		st.Retry = ""
		return
	}
	st.Status = status
	st.Reason = reason
	st.Retry = ""
}

// judge decides whether a claim is accepted, by the item's type (D5.3/D5.4).
func (c *Coverage) judge(st *ItemState, o StepObservation, r domain.ItemResult) (bool, domain.RequirementStatus, string) {
	// ADR-0145 D14: a write or document item that has an executed write is
	// satisfied whatever the worker claimed, and a refused target alongside it is
	// kept per target (st.Targets), never turning the item not_permitted.
	// The write must be one to the item's own target
	// (T-20261112-coverage-settles-a-write-item-on-any-done): another tool's
	// success (a create_directory beside a failed write_file) never settles it.
	if (st.Item.Type == domain.RequirementWrite || st.Item.Type == domain.RequirementDocument) && r.Status != domain.RequirementSatisfied {
		if a, ok := doneAction(st.targetActions(o)); ok {
			return true, domain.RequirementSatisfied, "executed by " + a.Tool + st.refusedTargetsNote()
		}
	}
	switch r.Status {
	case domain.RequirementSatisfied:
		switch st.Item.Type {
		case domain.RequirementAnswer, domain.RequirementFigure:
			// The item's expected value types (requirement_checklist v3 expects;
			// T-20261112-checklist-types-the-source-page-for-each): "the source page"
			// is answered by a citation, never by a figure. A figure item that
			// declares none keeps the figure types. Whether a figure VALUE came from
			// a tool result or a calculation is checked per value by the envelope
			// validator (D6), whatever the item expects.
			exp := itemExpects(st.Item)
			for _, v := range r.Values {
				if domain.ExpectsValueType(exp, v.Type) {
					return true, domain.RequirementSatisfied, ""
				}
			}
			if len(r.Values) == 0 {
				if st.Item.Type == domain.RequirementFigure && len(st.Item.Expects) == 0 {
					return false, "", "no number, money amount or date from a tool result or calculation"
				}
				return false, "", "satisfied was claimed with no value backed by evidence"
			}
			if st.Item.Type == domain.RequirementFigure && len(st.Item.Expects) == 0 {
				return false, "", "no number, money amount or date from a tool result or calculation (the values sent were " + valueTypesText(r.Values) + ")"
			}
			return false, "", "no value of the type the item expects (" + strings.Join(exp, ", ") + "); the values sent were " + valueTypesText(r.Values)
		case domain.RequirementDocument:
			for _, v := range r.Values {
				if o.EvidenceKinds[v.EvidenceRef] == domain.LedgerArtifact {
					return true, domain.RequirementSatisfied, ""
				}
			}
			if a, ok := doneAction(st.targetActions(o)); ok {
				return true, domain.RequirementSatisfied, "drafted by " + a.Tool + st.refusedTargetsNote()
			}
			// ADR-0145 D14.6: the draft is the step's own answer, kept in the run
			// ledger as its step output; nothing has to leave Cambrian for it.
			if strings.TrimSpace(o.AnswerText) != "" {
				return true, domain.RequirementSatisfied, "drafted in the step's answer (ledger step output)"
			}
			return false, "", "no draft: return the full text of the document as the answer"
		case domain.RequirementWrite:
			acts := st.targetActions(o)
			if a, ok := doneAction(acts); ok {
				return true, domain.RequirementSatisfied, "executed by " + a.Tool + st.refusedTargetsNote()
			}
			if a, ok := refusedAction(acts); ok {
				return true, domain.RequirementNotPermitted, "refused: " + firstNonEmpty(a.Detail, string(a.ApprovalState), a.Tool)
			}
			return false, "", st.noWriteReason(o, acts)
		}
	case domain.RequirementNotFound:
		switch {
		case o.ToolResults == 0:
			return false, "", "not_found was claimed without a search in the ledger"
		case o.ToolErrors >= o.ToolResults:
			st.erroredSearches++
			if st.erroredSearches >= 2 {
				return true, domain.RequirementNotFound, "searches errored on two attempts: " + r.Reason
			}
			st.Retry = "The searches for it returned errors: try a different strategy or source."
			return false, "", "every search for it returned an error"
		}
		return true, domain.RequirementNotFound, r.Reason
	case domain.RequirementNotPermitted:
		// A refused side-effecting call, or a refused read: both are the kernel's
		// own record (D5.3 "an approval denied or access refused").
		if a, ok := refusedAction(o.Actions); ok {
			return true, domain.RequirementNotPermitted, "refused: " + firstNonEmpty(a.Detail, string(a.ApprovalState), a.Tool)
		}
		if a, ok := refusedAction(o.Denials); ok {
			return true, domain.RequirementNotPermitted, "access refused: " + firstNonEmpty(a.Detail, string(a.ApprovalState), a.Tool)
		}
		return false, "", "not_permitted was claimed but the kernel recorded no refusal"
	case domain.RequirementFailed:
		return false, "", firstNonEmpty(r.Reason, "the worker reported failure")
	}
	return false, "", firstNonEmpty(r.Reason, "no accepted result")
}

// figureTypes are what a figure item that declares no expects takes.
var figureTypes = []string{domain.ValueNumber, domain.ValueMoney, domain.ValueDate}

// itemExpects is the value types an answer or figure item takes: its declared
// expects, else any type for an answer and the figure types for a figure.
func itemExpects(it domain.RequirementItem) []string {
	if len(it.Expects) > 0 {
		return it.Expects
	}
	if it.Type == domain.RequirementFigure {
		return figureTypes
	}
	return nil
}

// valueTypesText names the values a result sent with their types ("total
// (string)").
func valueTypesText(vs []domain.EnvelopeValue) string {
	var parts []string
	for _, v := range vs {
		parts = append(parts, v.Name+" ("+v.Type+")")
	}
	return strings.Join(parts, ", ")
}

// ObserveActions checks the constraints against the plan's side-effecting calls
// (D5.3 constraint: checked continuously). effectsOf maps a call to the effect
// terms it produced (without the classes its step needed for the request's own
// write items, ADR-0145 D14.6). A forbidden effect on a done or failed (attempted)
// call fails the constraint; a refused call did not run and violates nothing.
func (c *Coverage) ObserveActions(actions []domain.PlanAction, effectsOf func(a domain.PlanAction) []string) {
	if effectsOf == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.items {
		if st.Item.Type != domain.RequirementConstraint || len(st.Item.Forbids) == 0 || st.Status == domain.RequirementFailed {
			continue
		}
		for _, a := range actions {
			if a.Status == domain.PlanActionRefused {
				continue
			}
			for _, e := range effectsOf(a) {
				if containsStr(st.Item.Forbids, domain.NormalizeStepEffect(e)) {
					st.Status = domain.RequirementFailed
					st.Reason = fmt.Sprintf("violated: step %d called %s (%s)", a.Step, a.Tool, e)
					st.Steps = appendUniqueInt(st.Steps, a.Step)
				}
			}
		}
	}
}

// Status returns an item's current status ("" for an unknown id).
func (c *Coverage) Status(id string) domain.RequirementStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.byID[id]; st != nil {
		return st.Status
	}
	return ""
}

// EndStage compares the item states with the previous stage's (D5.5). Progress
// is any item changing status or gaining evidence or a covered member. A stage
// with no progress is a stall.
func (c *Coverage) EndStage() (progress bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stages++
	p := c.fingerprint()
	progress = p != c.lastPrint
	c.lastPrint = p
	return progress
}

// Stages is the number of stages ended.
func (c *Coverage) Stages() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stages
}

// Settled reports whether every item but the constraints is in a final state
// (D5.1: done = every item final; a constraint is held while nothing violates it).
func (c *Coverage) Settled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.items {
		if st.Item.Type != domain.RequirementConstraint && !st.Status.Final() {
			return false
		}
	}
	return true
}

// CloseGaps ends the request: every open item closes as a gap (failed) with the
// last reason the kernel had for it, prefixed by why the work stopped. A
// constraint nothing violated stays open with reason "held".
func (c *Coverage) CloseGaps(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = true
	for _, st := range c.items {
		if st.Status.Final() {
			continue
		}
		if st.Item.Type == domain.RequirementConstraint {
			st.Reason = "held: no step made a forbidden call"
			continue
		}
		reason := firstNonEmpty(st.Reason, "no step produced an accepted result")
		st.Status = domain.RequirementFailed
		st.Reason = "gap (" + why + "): " + reason
	}
}

// NextStage is what the next coverage stage's planner is told: the open items,
// their reasons, missing members and retry instructions, and the settled ids.
func (c *Coverage) NextStage(number int) *domain.CoverageStage {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &domain.CoverageStage{Number: number, SettledAs: map[string]domain.RequirementStatus{}}
	for _, st := range c.items {
		if st.Item.Type == domain.RequirementConstraint {
			continue
		}
		if st.Status.Final() {
			s.Settled = append(s.Settled, st.Item.ID)
			s.SettledAs[st.Item.ID] = st.Status
			continue
		}
		s.Open = append(s.Open, domain.OpenItem{ID: st.Item.ID, Reason: st.Reason, Missing: st.missing(), Retry: st.Retry})
	}
	return s
}

// Report is the coverage's final or current state.
type Report struct {
	Items   []ItemState `json:"items"`
	Stages  int         `json:"stages"`
	Settled bool        `json:"settled"`
	Stalled bool        `json:"stalled,omitempty"`
}

// Snapshot returns a copy of every item's state.
func (c *Coverage) Snapshot() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := Report{Stages: c.stages, Stalled: c.stalled, Settled: true}
	for _, st := range c.items {
		cp := *st
		cp.Values = append([]ItemValue(nil), st.Values...)
		cp.Evidence = append([]string(nil), st.Evidence...)
		cp.Members = append([]string(nil), st.Members...)
		cp.Targets = append([]TargetResult(nil), st.Targets...)
		if st.Covered != nil {
			cp.Covered = map[string]bool{}
			for k, v := range st.Covered {
				cp.Covered[k] = v
			}
		}
		if st.Item.Type != domain.RequirementConstraint && !st.Status.Final() {
			r.Settled = false
		}
		r.Items = append(r.Items, cp)
	}
	return r
}

func (c *Coverage) fingerprint() string {
	var b strings.Builder
	for _, st := range c.items {
		covered := make([]string, 0, len(st.Covered))
		for m := range st.Covered {
			covered = append(covered, m)
		}
		sort.Strings(covered)
		// Only accepted values and their evidence are progress; a refused value
		// changes nothing the judge decided.
		var values int
		var evidence []string
		for _, v := range st.Values {
			if !v.Refused {
				values++
				evidence = appendUniqueStr(evidence, v.EvidenceRef)
			}
		}
		fmt.Fprintf(&b, "%s=%s|%d|%d|%s|%s;", st.Item.ID, st.Status, values, len(evidence),
			strings.Join(covered, ","), st.Conflict)
	}
	return b.String()
}

// observeTargets folds a step's side-effecting calls into the item's per-target
// outcomes. A target executed once stays satisfied; a later refusal or failure of
// the same target (a duplicate the kernel refused) does not change it.
func (st *ItemState) observeTargets(o StepObservation) {
	for _, a := range o.Actions {
		key := a.Target
		if key == "" {
			key = a.Tool + "#" + strings.Join(a.IDs, ",")
		}
		status := domain.RequirementFailed
		switch a.Status {
		case domain.PlanActionDone:
			status = domain.RequirementSatisfied
		case domain.PlanActionRefused:
			status = domain.RequirementNotPermitted
		}
		found := false
		for i := range st.Targets {
			t := &st.Targets[i]
			if t.Target != key {
				continue
			}
			found = true
			if t.Status != domain.RequirementSatisfied {
				t.Status, t.Step, t.IDs, t.Detail, t.Tool = status, o.Step, a.IDs, a.Detail, a.Tool
			}
		}
		if !found {
			st.Targets = append(st.Targets, TargetResult{Target: key, Tool: a.Tool, Status: status, Step: o.Step, IDs: a.IDs, Detail: a.Detail})
		}
	}
}

// targetActions are the step's side-effecting calls that can carry out the write
// or document item st: the per-target effect record (kind = write, target, status)
// matched against the targets the item DECLARES (ADR-0145 D5.3/D14;
// requirement_checklist v3, T-20261112-write-target-coverage-reads-title-pdf-from-a).
// Every action in the plan action ledger is already write-class: the ledger
// records only calls whose tool the effect classification marks as changing the
// world (SystemTool.SideEffecting, network/plan_actions.go). What is checked here
// is WHICH target was written, so a done create_directory never settles "save the
// CSV at …/pricing.csv" while every write_file to that path failed.
//
// The item's targets come from the deriver (RequirementItem.Targets), validated
// in code at derivation: an exact value is verbatim in the request, a glob
// compiles. When the item declares any, only the calls whose target matches one
// of them count. When it declares none (the target is unknown: "save it in the
// archive"), every write-class call of the step counts. The item's text is never
// parsed for targets: free text mixes literal names with templates and examples
// ("in the form \"YYYY-MM-DD Title.pdf\"" once made title.pdf the target), and
// code cannot tell them apart. The worker's claim never decides; only the
// kernel's record of the call does.
func (st *ItemState) targetActions(o StepObservation) []domain.PlanAction {
	if len(st.Item.Targets) == 0 {
		return o.Actions
	}
	var named []domain.PlanAction
	for _, a := range o.Actions {
		if actionMatches(a, st.Item.Targets) {
			named = append(named, a)
		}
	}
	return named
}

// noWriteReason says why a write item is still open, for the next stage's planner:
// the item's declared targets, and what the step's calls did and wrote instead, so
// a mismatch between the declared target and the written one is visible.
func (st *ItemState) noWriteReason(o StepObservation, acts []domain.PlanAction) string {
	declared := targetsText(st.Item.Targets)
	if len(acts) == 0 {
		if len(o.Actions) > 0 && declared != "" {
			var others []string
			for _, a := range o.Actions {
				s := a.Tool + " " + a.Status
				if v := domain.TargetValue(a.Target); v != "" {
					s += " (wrote " + v + ")"
				}
				others = append(others, s)
			}
			return fmt.Sprintf("no executed write to %s in the action ledger (the step's other calls did not match it: %s)",
				declared, strings.Join(others, "; "))
		}
		return "no executed write in the action ledger"
	}
	last := acts[len(acts)-1]
	what := "the write"
	if declared != "" {
		what = "the write to " + declared
	}
	return fmt.Sprintf("%s did not execute: %s %s (%s); it must be done again", what, last.Tool, last.Status,
		firstNonEmpty(last.Detail, "no detail"))
}

// targetsText renders declared targets for a reason ("C:/out/pricing.csv",
// "Receipts/????-??-?? *.pdf (pattern)").
func targetsText(ts []domain.ItemTarget) string {
	var parts []string
	for _, t := range ts {
		s := t.Value
		if t.Match == domain.TargetMatchGlob {
			s += " (pattern)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

var targetEmailRe = regexp.MustCompile(`[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)

// normTarget normalises a target value or pattern for comparison: lower case,
// whitespace folded (the write record folds it too), Windows separators as
// forward slashes, no trailing separator.
func normTarget(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(s, `\`, "/")), " "))
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
	}
	return s
}

// actionMatches reports whether a call reached one of the declared targets: its
// write-record target (domain.WriteTargets.Key; a move/rename/copy records its
// destination), else, for a tool no rule names a target for, any of its argument
// values.
func actionMatches(a domain.PlanAction, targets []domain.ItemTarget) bool {
	if a.Target != "" {
		return valueMatches(domain.TargetValue(a.Target), targets)
	}
	for _, v := range a.Args {
		if valueMatches(v, targets) {
			return true
		}
	}
	return false
}

// valueMatches reports whether a target value (one value, or a list joined by
// commas as the write record keeps one) matches a declared target.
func valueMatches(value string, targets []domain.ItemTarget) bool {
	values := []string{value}
	if strings.Contains(value, ",") {
		values = append(values, strings.Split(value, ",")...)
	}
	for _, raw := range values {
		v := normTarget(raw)
		if v == "" {
			continue
		}
		for _, t := range targets {
			if targetMatches(t, v) {
				return true
			}
		}
	}
	return false
}

// targetMatches reports whether a normalised value v matches declared target t.
//   - exact: the same value; the same address (recipient); for a file or folder
//     also the same path written relative or absolute (one ends with the other at
//     a separator); a folder is also matched by anything written inside it.
//   - glob: path.Match of the pattern against the value, or against the value's
//     last segments when the pattern is a relative path ("Receipts/*.pdf" matches
//     "c:/users/demo/documents/receipts/x.pdf"); for a folder, also against the
//     folder the value is in.
func targetMatches(t domain.ItemTarget, v string) bool {
	p := normTarget(t.Value)
	if p == "" {
		return false
	}
	if t.Match == domain.TargetMatchGlob {
		cands := []string{v}
		if t.Kind == domain.TargetKindFolder {
			cands = append(cands, path.Dir(v))
		}
		for _, c := range cands {
			if globMatches(p, c) {
				return true
			}
		}
		return false
	}
	switch {
	case v == p:
		return true
	case t.Kind == domain.TargetKindRecipient || targetEmailRe.MatchString(p):
		addr := targetEmailRe.FindString(v)
		return addr != "" && addr == targetEmailRe.FindString(p)
	case t.Kind == domain.TargetKindRecord:
		return false
	}
	if pathSuffix(v, p) || pathSuffix(p, v) {
		return true
	}
	if t.Kind == domain.TargetKindFolder {
		// Something written inside the folder: its parent path ends with the folder.
		for d := path.Dir(v); d != "." && d != "/"; {
			if d == p || pathSuffix(d, p) {
				return true
			}
			nd := path.Dir(d)
			if nd == d {
				break
			}
			d = nd
		}
	}
	return false
}

// pathSuffix reports whether long ends with short at a path separator.
func pathSuffix(long, short string) bool {
	short = strings.TrimPrefix(short, "/")
	return short != "" && strings.HasSuffix(long, "/"+short)
}

// globMatches matches pattern against v, or against v's last segments when the
// pattern has fewer segments than v (a relative pattern).
func globMatches(pattern, v string) bool {
	if ok, err := path.Match(pattern, v); err == nil && ok {
		return true
	}
	ps := strings.Split(pattern, "/")
	vs := strings.Split(v, "/")
	if len(ps) >= len(vs) {
		return false
	}
	ok, err := path.Match(pattern, strings.Join(vs[len(vs)-len(ps):], "/"))
	return err == nil && ok
}

// refusedTargetsNote names the targets that were not written, for the reason of
// an item satisfied by its other targets.
func (st *ItemState) refusedTargetsNote() string {
	var parts []string
	for _, t := range st.Targets {
		if t.Status != domain.RequirementSatisfied {
			parts = append(parts, domain.TargetValue(t.Target)+" "+string(t.Status))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (not written: " + strings.Join(parts, "; ") + ")"
}

// coverMembersByWrites is D5.6 for a write done by ONE ordinary step over a set
// (T-20261109-plan-identifies-4-october-receipts-but-the-move: a step moved 3 of the
// 4 receipts its fan-out inputs named, and one done move_file settled the item, so
// the reply said they were all moved). When a done write of the step names a member
// of the set it consumed (an identifier of the member in the call's arguments), the
// writes are per member: the whole set becomes the item's members and each member
// is covered only by a done write naming it, so a member with no write keeps the
// item open. A write naming no member (one folder created for all of them) is not
// per member and leaves the item as before. Members of earlier stages are covered
// by later writes too.
func (st *ItemState) coverMembersByWrites(o StepObservation) {
	var args []string
	for _, a := range o.Actions {
		if a.Status != domain.PlanActionDone {
			continue
		}
		for _, v := range a.Args {
			args = append(args, strings.ToLower(v))
		}
	}
	if len(args) == 0 {
		return
	}
	named := func(member string) bool {
		for _, tok := range memberTokens(member) {
			for _, a := range args {
				if strings.Contains(a, tok) {
					return true
				}
			}
		}
		return false
	}
	perMember := false
	for _, m := range o.SetMembers {
		if named(m) {
			perMember = true
			break
		}
	}
	if perMember {
		for _, m := range o.SetMembers {
			if m != "" && !containsStr(st.Members, m) {
				st.Members = append(st.Members, m)
			}
		}
	}
	for _, m := range st.Members {
		if !st.Covered[m] && named(m) {
			if st.Covered == nil {
				st.Covered = map[string]bool{}
			}
			st.Covered[m] = true
		}
	}
}

// memberTokens are the identifiers a fan-out member names: tokens with a letter and
// a digit, dot or underscore ("scan_0041.pdf", "MSG-000082", "INV-7"), lowercased.
// A member with none (plain words) is named by its whole text.
func memberTokens(member string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(member, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;()\"'[]{}/\\", r)
	}) {
		f = strings.ToLower(strings.TrimRight(f, ".:"))
		if len(f) < 4 || !strings.ContainsAny(f, "abcdefghijklmnopqrstuvwxyz") ||
			!strings.ContainsAny(f, "0123456789._") {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		if t := strings.ToLower(strings.TrimSpace(member)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (st *ItemState) missing() []string {
	var out []string
	for _, m := range st.Members {
		if !st.Covered[m] {
			out = append(out, m)
		}
	}
	return out
}

// conflictWith finds an accepted value of the same name for the same member,
// from another step, that disagrees with v.
// A refused value contests nothing: it was never accepted.
func (st *ItemState) conflictWith(v domain.EnvelopeValue, o StepObservation) (ItemValue, bool) {
	for _, p := range st.Values {
		if !p.Refused && p.Name == v.Name && p.Member == o.Member && p.Step != o.Step && !sameValue(p.EnvelopeValue, v) {
			return p, true
		}
	}
	return ItemValue{}, false
}

// valueIndex is the index of the stored value equal to v (same name, member and
// value), or -1.
func (st *ItemState) valueIndex(v domain.EnvelopeValue, o StepObservation) int {
	for i, p := range st.Values {
		if p.Name == v.Name && p.Member == o.Member && sameValue(p.EnvelopeValue, v) {
			return i
		}
	}
	return -1
}

// sameValue compares two values by meaning where the type allows: numbers by
// amount, dates by day, everything else by trimmed text (ids case-insensitively).
func sameValue(a, b domain.EnvelopeValue) bool {
	switch {
	case (a.Type == domain.ValueNumber || a.Type == domain.ValueMoney) && (b.Type == domain.ValueNumber || b.Type == domain.ValueMoney):
		x, ok1 := domain.ParseAmount(a.Text())
		y, ok2 := domain.ParseAmount(b.Text())
		if ok1 && ok2 {
			d := x - y
			return d < 0.005 && d > -0.005
		}
	case a.Type == domain.ValueDate && b.Type == domain.ValueDate:
		x, ok1 := domain.ParseDateValue(a.Text())
		y, ok2 := domain.ParseDateValue(b.Text())
		if ok1 && ok2 {
			return x.Equal(y)
		}
	case a.Type == domain.ValueID || b.Type == domain.ValueID:
		return strings.EqualFold(strings.TrimSpace(a.Text()), strings.TrimSpace(b.Text()))
	}
	return strings.TrimSpace(a.Text()) == strings.TrimSpace(b.Text())
}

func doneAction(as []domain.PlanAction) (domain.PlanAction, bool) {
	for _, a := range as {
		if a.Status == domain.PlanActionDone {
			return a, true
		}
	}
	return domain.PlanAction{}, false
}

func refusedAction(as []domain.PlanAction) (domain.PlanAction, bool) {
	for _, a := range as {
		if a.Status == domain.PlanActionRefused {
			return a, true
		}
	}
	return domain.PlanAction{}, false
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func appendUniqueStr(xs []string, x string) []string {
	if x == "" || containsStr(xs, x) {
		return xs
	}
	return append(xs, x)
}

func appendUniqueInt(xs []int, x int) []int {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
