package domain

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Request-tied guards (ADR-0145 D14; T-20261109-adr-0145-d14-fix-batch-remove-per-step).
//
// The replay gates showed that a per-STEP permission (the step effect vocabulary
// and effect-filtered tool menus) refused legitimate tools and still let the real
// failures through: a re-worded write to a recipient already written, a repair
// turn re-running a write, a write nobody asked for. The owner removed the per-step
// permission layer (security is the person's own access and approvals) and asked
// for guards tied to the REQUEST instead, enforced by the ToolExecutor at call time:
//
//  1. the person's "don't" constraints from the requirement checklist, by ADR-0086
//     tool class (forbids: send ⇒ every tool that may send, class egress_send,
//     is refused; ENG-1426);
//  2. a per-target write record for the request (the ordering conversation turn, or
//     the root run when there is no turn): a target written once cannot be written
//     again in that request, whatever the wording; a repair turn never re-runs a
//     write its step already executed;
//  3. a write no open write/document item asked for goes to approval with a notice
//     ("this write was not part of the request"), like the duplicate notice.
//
// The facts a guard needs (the request scope, the constraints, the write items the
// step answers) are bound to the step's lease by the kernel at dispatch (StepGuard
// on LeaseBinding) and reach the executor on ctx (StepCall). Nothing here comes
// from an agent.
// ─────────────────────────────────────────────────────────────────────────────

// StepGuard is what the kernel knows about the request a plan step works for.
// Written by the kernel at dispatch, never by an agent.
type StepGuard struct {
	// Request is the scope of the per-target write record: "turn:<conversation>/<turn>"
	// for work a chat turn ordered, else "run:<root run id>" (coverage stages and
	// replans share their root). "" disables the write record for the step.
	Request string
	// Checklist is true when the request has a derived requirement checklist; the
	// unrequested-write guard applies only then.
	Checklist bool
	// Constraints are the checklist's "don't" constraints, resolved to tool classes.
	Constraints []ConstraintGuard
	// WriteItems are the open WRITE items the step answers. Only a write item may
	// put anything into an outside system (ADR-0145 D14.6): with a checklist and no
	// write item, a side-effecting call is not part of the request.
	WriteItems []string
	// Documents are the open DOCUMENT items the step answers. A document is
	// satisfied inside Cambrian (the draft is the step's answer, a ledger entry), so
	// a step that answers documents and no write item has every side-effecting call
	// refused with that reason (T-20261109-d14-lets-a-drafting-step-answers-only-the).
	Documents []string
	// Carries are the tool classes the step's write items themselves need: the
	// write class for any write item, plus the classes of each item's Does terms.
	// A checklist constraint never refuses one of these classes on this step: it
	// stops other actions of that kind, never the request's own write
	// (ADR-0145 D14.6; T-20261109-d14-constraint-forbids-change-refuses-the-send).
	Carries []ToolEffect
	// Repair marks the step's envelope repair turn (D6): a write the step already
	// executed is never run again in it.
	Repair bool
}

// ConstraintGuard is one checklist constraint, resolved to the ADR-0086 tool
// classes it rules out.
type ConstraintGuard struct {
	Item    string       `json:"item"`
	Quote   string       `json:"quote"`
	Terms   []string     `json:"terms"`
	Classes []ToolEffect `json:"classes"`
}

// ResolveConstraints maps each constraint item's forbids terms to tool classes
// through terms (execution.plan.constraint_classes). A term the map does not know
// is dropped (the deriver offers only the map's terms).
func ResolveConstraints(c *RequirementChecklist, terms map[string][]ToolEffect) []ConstraintGuard {
	var out []ConstraintGuard
	for _, it := range c.Constraints() {
		g := ConstraintGuard{Item: it.ID, Quote: it.Quote}
		seen := map[ToolEffect]bool{}
		for _, f := range it.Forbids {
			n := NormalizeStepEffect(f)
			cls, ok := terms[n]
			if !ok {
				continue
			}
			g.Terms = append(g.Terms, n)
			for _, c := range cls {
				if !seen[c] {
					seen[c] = true
					g.Classes = append(g.Classes, c)
				}
			}
		}
		if len(g.Classes) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ForbiddenBy returns the constraint that rules out a call of t and the class it
// matched. A call is ruled out when it exercises a constraint class (Exercises:
// the tool's CeilingEffects covered by the class — the same classification the
// Rule of Two and the outbound identifier guard use, ENG-1426), so `forbids: send`
// (class egress_send) refuses a tool declaring egress_send or plain egress and
// never one declaring egress_read alone. An effect a class in carries covers (the
// classes the step's own write items need, StepGuard.Carries) is never ruled out:
// a constraint stops other actions of that kind, not the request's own write
// (ADR-0145 D14.6).
func ForbiddenBy(t SystemTool, cons []ConstraintGuard, carries ...ToolEffect) (ConstraintGuard, ToolEffect, bool) {
	for _, c := range cons {
		for _, cl := range c.Classes {
			if ExercisesUncarried(t, cl, carries) {
				return c, cl, true
			}
		}
	}
	return ConstraintGuard{}, "", false
}

// CarriedClasses are the tool classes the given write items need (StepGuard.Carries):
// the write class for any write item, plus the classes their Does terms map to
// through terms (execution.tools.constraint_classes). Sorted, unique; nil for no
// write item.
func CarriedClasses(items []RequirementItem, terms map[string][]ToolEffect) []ToolEffect {
	seen := map[ToolEffect]bool{}
	for _, it := range items {
		if it.Type != RequirementWrite {
			continue
		}
		seen[EffectWrite] = true
		for _, d := range it.Does {
			for _, cl := range terms[NormalizeStepEffect(d)] {
				seen[cl] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]ToolEffect, 0, len(seen))
	for cl := range seen {
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DocumentWriteRefusal words the refusal of a side-effecting call on a step that
// answers document items and no write item (ADR-0145 D14.6).
func DocumentWriteRefusal(tool string, documents []string) string {
	return fmt.Sprintf("%s is refused: this step drafts %s, and a draft is finished inside Cambrian: return the full text "+
		"as your answer (it is kept in the ledger). Nothing goes into an outside system from a drafting step; "+
		"only a step that answers a write item of the request may create, send, file or record anything.",
		tool, strings.Join(documents, ", "))
}

// ConstraintRefusal words the refusal of a call a constraint rules out.
func ConstraintRefusal(tool string, c ConstraintGuard, class ToolEffect) string {
	return fmt.Sprintf("%s is refused: the person said %q (%s), which rules out %s actions in this request. "+
		"Do not call any %s tool; finish the rest of the work and report this part as not done because of that instruction.",
		tool, c.Quote, c.Item, class, class)
}

// ── write targets ───────────────────────────────────────────────────────────

// WriteTargetRule names, for the tools whose base names match one of Tools, the
// argument fields whose values identify what a write reaches (execution.tools.
// write_targets.rules).
type WriteTargetRule struct {
	Tools []string
	Keys  []string
}

// WriteTargets derives a side-effecting call's target deterministically from its
// arguments: the first rule whose tool globs match the tool and whose keys name a
// present argument, else the default keys. A call none of them names has no target
// (only the D9 idempotency key and the duplicate notice apply to it).
type WriteTargets struct {
	Rules       []WriteTargetRule
	DefaultKeys []string
}

var emailRe = regexp.MustCompile(`[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)

// Key returns the call's target ("<tool base>|<field>=<value>"), or "" when the call
// names none. Values are normalised (case, whitespace, list order); a value holding
// e-mail addresses is reduced to the addresses, so "Brisa <brisa@x.com>" and
// "brisa@x.com" are the same recipient.
func (w *WriteTargets) Key(tool string, args []byte) string {
	if w == nil {
		return ""
	}
	leaves := normaliseActionArgs(args).leaves
	if len(leaves) == 0 {
		return ""
	}
	base := toolBaseName(tool)
	pick := func(keys []string) string {
		for _, k := range keys {
			v, ok := leaves[strings.ToLower(k)]
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			if ms := emailRe.FindAllString(v, -1); len(ms) > 0 {
				sort.Strings(ms)
				v = strings.Join(uniqueSorted(ms), ",")
			}
			return base + "|" + strings.ToLower(k) + "=" + v
		}
		return ""
	}
	for _, r := range w.Rules {
		matched := false
		for _, g := range r.Tools {
			if MatchToolGlob(g, base) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if k := pick(r.Keys); k != "" {
			return k
		}
	}
	return pick(w.DefaultKeys)
}

func uniqueSorted(xs []string) []string {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// WrittenTarget is one target a request wrote.
type WrittenTarget struct {
	Target string
	Tool   string
	Run    RunID
	Step   int
	// IDs are what the write returned (PlanActionIDs); LedgerRef its ledger entry.
	IDs       []string
	LedgerRef string
	done      bool
}

// WriteRecord is the per-request record of written targets (D14 guard 2). In
// memory and bounded by a TTL, like the idempotency cache: the request it guards
// does not survive a restart either. Safe for concurrent use; nil records nothing.
type WriteRecord struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	reqs map[string]*requestWrites
}

type requestWrites struct {
	at      time.Time
	targets map[string]*WrittenTarget
	// steps holds each (run, step)'s executed side-effecting calls, targeted or not
	// (the repair-turn guard).
	steps map[string][]WrittenTarget
}

// NewWriteRecord keeps a request's writes for ttl after its last write (<= 0: 24h).
func NewWriteRecord(ttl time.Duration) *WriteRecord {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &WriteRecord{ttl: ttl, now: time.Now, reqs: map[string]*requestWrites{}}
}

func stepKey(run RunID, step int) string { return fmt.Sprintf("%s#%d", run, step) }

func (r *WriteRecord) request(scope string) *requestWrites {
	now := r.now()
	for k, rw := range r.reqs {
		if now.Sub(rw.at) > r.ttl {
			delete(r.reqs, k)
		}
	}
	rw := r.reqs[scope]
	if rw == nil {
		rw = &requestWrites{targets: map[string]*WrittenTarget{}, steps: map[string][]WrittenTarget{}}
		r.reqs[scope] = rw
	}
	rw.at = now
	return rw
}

// Reserve claims target for a call about to run. It returns the earlier write
// (written=true, or in flight) when the target is taken; otherwise release must be
// called once with the outcome: ok=true keeps the target written, false frees it
// (a refused or failed call wrote nothing).
func (r *WriteRecord) Reserve(scope, target string, w WrittenTarget) (prior WrittenTarget, taken bool, release func(ok bool, ids []string, ledgerRef string)) {
	if r == nil || scope == "" || target == "" {
		return WrittenTarget{}, false, func(bool, []string, string) {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rw := r.request(scope)
	if p, ok := rw.targets[target]; ok {
		return *p, true, nil
	}
	w.Target = target
	entry := &w
	rw.targets[target] = entry
	return WrittenTarget{}, false, func(ok bool, ids []string, ledgerRef string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !ok {
			if rw.targets[target] == entry {
				delete(rw.targets, target)
			}
			return
		}
		entry.done, entry.IDs, entry.LedgerRef = true, ids, ledgerRef
	}
}

// RecordStepWrite notes an executed side-effecting call of (run, step).
func (r *WriteRecord) RecordStepWrite(scope string, w WrittenTarget) {
	if r == nil || scope == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rw := r.request(scope)
	k := stepKey(w.Run, w.Step)
	w.done = true
	rw.steps[k] = append(rw.steps[k], w)
}

// StepWrites returns the side-effecting calls (run, step) executed in scope.
func (r *WriteRecord) StepWrites(scope string, run RunID, step int) []WrittenTarget {
	if r == nil || scope == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rw := r.reqs[scope]
	if rw == nil {
		return nil
	}
	return append([]WrittenTarget(nil), rw.steps[stepKey(run, step)]...)
}

// Targets returns every target the request has written (done), sorted.
func (r *WriteRecord) Targets(scope string) []WrittenTarget {
	if r == nil || scope == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rw := r.reqs[scope]
	if rw == nil {
		return nil
	}
	var out []WrittenTarget
	for _, t := range rw.targets {
		if t.done {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// TargetValue is the value part of a target key ("brisa@x.com" of
// "mail_draft|to=brisa@x.com").
func TargetValue(target string) string {
	if _, v, ok := strings.Cut(target, "="); ok {
		return v
	}
	return target
}

// DuplicateTargetRefusal words the refusal of a write to a target the request
// already wrote (or is writing).
func DuplicateTargetRefusal(tool string, prior WrittenTarget) string {
	if !prior.done {
		return fmt.Sprintf("%s is refused: a write to the same target (%s) is already running in this request; "+
			"a target is written once per request. Do not retry it.", tool, prior.Target)
	}
	what := fmt.Sprintf("%s (step %d of run %s", prior.Tool, prior.Step, prior.Run)
	if len(prior.IDs) > 0 {
		what += ", result " + strings.Join(prior.IDs, ", ")
	}
	if prior.LedgerRef != "" {
		what += ", ledger " + prior.LedgerRef
	}
	what += ")"
	return fmt.Sprintf("%s is refused: %s was already written in this request by %s. A target is written once per request, "+
		"whatever the wording: do not write it again; report the earlier result instead.", tool, TargetValue(prior.Target), what)
}

// RepairWriteRefusal words the refusal of a write in a repair turn whose step
// already executed one.
func RepairWriteRefusal(tool string, done []WrittenTarget) string {
	parts := make([]string, 0, len(done))
	for _, d := range done {
		p := d.Tool
		if len(d.IDs) > 0 {
			p += " → " + strings.Join(d.IDs, ", ")
		}
		if d.LedgerRef != "" {
			p += " [" + d.LedgerRef + "]"
		}
		parts = append(parts, p)
	}
	return fmt.Sprintf("%s is refused: this is the repair turn of a step whose write already executed (%s). "+
		"The repair turn only corrects the answer envelope: cite the earlier result and do not write again.",
		tool, strings.Join(parts, "; "))
}

// UnrequestedWriteNoticePrefix opens the approval notice of a write no open write
// item of the request asked for (D14 guard 3).
const UnrequestedWriteNoticePrefix = "This write was not part of the request"

// UnrequestedWriteNotice words that notice for the approver.
func UnrequestedWriteNotice(tool string) string {
	return UnrequestedWriteNoticePrefix + ": no open write item of the person's request asks for it (" +
		tool + "). Approve only if the person wants it."
}

// ── repeated refusals (D14.5) ───────────────────────────────────────────────

// RefusalRecord remembers the refused calls of each step (or lease, outside a
// plan step) by (tool, canonical arguments), so an identical call is answered with
// the same refusal without running the checks, an approval or the tool again.
// Bounded per scope; nil records nothing.
type RefusalRecord struct {
	mu     sync.Mutex
	ttl    time.Duration
	now    func() time.Time
	scopes map[string]*refusals
}

type refusals struct {
	at    time.Time
	calls map[string]ToolCallResponse
	order []string
}

const refusalsPerScope = 64

// NewRefusalRecord keeps a scope's refusals for ttl after its last refusal (<= 0: 6h).
func NewRefusalRecord(ttl time.Duration) *RefusalRecord {
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &RefusalRecord{ttl: ttl, now: time.Now, scopes: map[string]*refusals{}}
}

func refusalKey(tool string, args []byte) string { return tool + "\x00" + string(CanonicalJSON(args)) }

// RefusalScope is a call's refusal scope: its plan step, else its lease.
func RefusalScope(ctx context.Context, lease LeaseID) string {
	if sc, ok := StepCallFrom(ctx); ok {
		return "step:" + stepKey(sc.RunID, sc.StepIndex)
	}
	if lease != "" {
		return "lease:" + string(lease)
	}
	return ""
}

// Lookup returns the earlier refusal of the identical call in scope.
func (r *RefusalRecord) Lookup(scope, tool string, args []byte) (ToolCallResponse, bool) {
	if r == nil || scope == "" {
		return ToolCallResponse{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.scopes[scope]
	if s == nil || r.now().Sub(s.at) > r.ttl {
		return ToolCallResponse{}, false
	}
	resp, ok := s.calls[refusalKey(tool, args)]
	return resp, ok
}

// Record stores a refusal.
func (r *RefusalRecord) Record(scope, tool string, args []byte, resp ToolCallResponse) {
	if r == nil || scope == "" || !resp.Denied {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, s := range r.scopes {
		if now.Sub(s.at) > r.ttl {
			delete(r.scopes, k)
		}
	}
	s := r.scopes[scope]
	if s == nil {
		s = &refusals{calls: map[string]ToolCallResponse{}}
		r.scopes[scope] = s
	}
	s.at = now
	k := refusalKey(tool, args)
	if _, seen := s.calls[k]; !seen {
		s.order = append(s.order, k)
		if len(s.order) > refusalsPerScope {
			delete(s.calls, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.calls[k] = resp
}

// RepeatedRefusalNote is appended to a refusal answered from the record.
const RepeatedRefusalNote = " (This identical call was already refused in this step and was not run again: " +
	"calling it again changes nothing. Do something else, or report this part as not done with the reason above.)"

// RequestGuards bundles the D14 guard state the ToolExecutor holds
// (execution.tools.request_guards). nil ⇒ no request-tied guards.
type RequestGuards struct {
	Targets  *WriteTargets
	Writes   *WriteRecord
	Refusals *RefusalRecord
}

type repairTurnKey struct{}

// WithRepairTurn marks ctx as a step's envelope repair turn (ADR-0145 D6/D14):
// the kernel binds it to the repair call's lease (StepGuard.Repair). Kernel-side
// only; it never crosses the wire.
func WithRepairTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, repairTurnKey{}, true)
}

// RepairTurnFrom reports whether ctx is a step's repair turn.
func RepairTurnFrom(ctx context.Context) bool {
	v, _ := ctx.Value(repairTurnKey{}).(bool)
	return v
}
