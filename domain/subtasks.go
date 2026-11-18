package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// ADR-0150 D2/D5/D6: kernel-run subtasks.
//
// The single-agent lane's agent fans out with the kernel tool run_subtasks
// (internal/subtasks). Each subtask runs on a fresh worker bound to a CHILD lease
// of the parent's session and run. This registry is the kernel's record of those
// leases and the enforcement point the ToolExecutor reads:
//
//   - the tool ceiling: a child lease may call only the tools it was composed
//     with (a LeaseToolCeiling, chained into ToolExecutor.LeaseCeiling);
//   - the EFFECT ceiling: every call on a child lease must stay inside the
//     lease's allowed effects (default read + egress_read). The ToolExecutor
//     checks it ITSELF, whatever the authorizer says — the OSS authorizer is
//     AllowAllAuthorizer, so the ADR-0086 effect gate alone would not hold;
//   - the depth: run_subtasks is allowed only to a registered parent (depth 0)
//     or to a child below max_depth;
//   - deterministic idempotency keys for a subtask's side-effecting calls (the
//     subtask id + the call's canonical arguments + its occurrence), so a resumed
//     subtask never repeats a write (AGENTS.md §2.6);
//   - what the worker did, observed by the kernel (its tool results' ledger ids,
//     whether it read the web), never claimed by the worker.
//
// Parents are registered by the lane that owns them (the single-agent lane binds
// its lease here); a planner-lane worker is never registered, so it is neither
// offered nor allowed run_subtasks (ADR-0150 Q6).
// ─────────────────────────────────────────────────────────────────────────────

// SubtaskBinding is what the kernel bound to one subtask's child lease.
type SubtaskBinding struct {
	// SubtaskID is the deterministic id (SubtaskIDFor): an identical retried
	// call is the same subtask.
	SubtaskID string
	// Label is the parent's label for the subtask (the brief's id), shown to the
	// person on an approval the subtask raises.
	Label string
	// ParentLease is the lease of the agent that spawned it; RootLease the lease
	// of the depth-0 parent (the per-request spawn cap counts there).
	ParentLease LeaseID
	RootLease   LeaseID
	// Depth is 1 for a subtask of the depth-0 agent, 2 for its child, ...
	Depth int
	// AllowedEffects is the lease's effect ceiling (never wider than its parent's).
	AllowedEffects []ToolEffect
	// Tools are the tool names the child may call (sorted not required).
	Tools []string
}

type subtaskState struct {
	b        SubtaskBinding
	tools    map[string]bool
	evidence []string
	readWeb  bool
	calls    map[string]int
}

type subtaskParent struct {
	agentID string
	spawned int
}

// Subtasks is the registry of subtask parents and child leases. Safe for
// concurrent use; a nil *Subtasks binds nothing and allows nothing.
type Subtasks struct {
	mu       sync.Mutex
	parents  map[string]*subtaskParent
	children map[string]*subtaskState
	idem     *IdempotencyCache
}

// NewSubtasks builds an empty registry; side-effecting subtask calls are kept
// for idemTTL (<= 0: 24 hours) so a resume within it never repeats one.
//
// actions is the ADR-0154 durable action record (ENG-1428): with it, every
// side-effecting subtask call is recorded proposed → dispatched → accepted under
// its deterministic key, and a resume after a kernel restart is answered from
// the record. nil keeps the results in memory only (no database, or migration
// 0046 not applied), exactly as before.
func NewSubtasks(idemTTL time.Duration, actions ActionRecordStore) *Subtasks {
	return &Subtasks{
		parents:  map[string]*subtaskParent{},
		children: map[string]*subtaskState{},
		idem:     NewDurableIdempotencyCache(idemTTL, actions),
	}
}

var _ LeaseToolCeiling = (*Subtasks)(nil)

// RegisterParent marks lease as the lease of a depth-0 agent that may spawn
// subtasks (the single-agent lane's agent). Kernel-only; never from the wire.
func (r *Subtasks) RegisterParent(lease LeaseID, agentID string) {
	if r == nil || lease == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.parents[string(lease)]; ok {
		p.agentID = agentID
		return
	}
	r.parents[string(lease)] = &subtaskParent{agentID: agentID}
}

// ForgetParent drops a parent lease (its run ended).
func (r *Subtasks) ForgetParent(lease LeaseID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.parents, string(lease))
}

// Bind records a child lease.
func (r *Subtasks) Bind(child LeaseID, b SubtaskBinding) {
	if r == nil || child == "" {
		return
	}
	tools := make(map[string]bool, len(b.Tools))
	for _, t := range b.Tools {
		tools[t] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.children[string(child)] = &subtaskState{b: b, tools: tools, calls: map[string]int{}}
}

// Forget drops a child lease (its subtask ended).
func (r *Subtasks) Forget(child LeaseID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.children, string(child))
}

// Binding returns the child lease's binding.
func (r *Subtasks) Binding(lease LeaseID) (SubtaskBinding, bool) {
	if r == nil || lease == "" {
		return SubtaskBinding{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.children[string(lease)]
	if !ok {
		return SubtaskBinding{}, false
	}
	return s.b, true
}

// IsChild reports whether lease is a subtask's child lease.
func (r *Subtasks) IsChild(lease LeaseID) bool {
	_, ok := r.Binding(lease)
	return ok
}

// Position reports where lease stands: depth 0 for a registered parent, the
// child's depth for a subtask lease; ok=false for any other lease (it may not
// spawn subtasks at all).
func (r *Subtasks) Position(lease LeaseID) (depth int, root LeaseID, ok bool) {
	if r == nil || lease == "" {
		return 0, "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, p := r.parents[string(lease)]; p {
		return 0, lease, true
	}
	if s, c := r.children[string(lease)]; c {
		return s.b.Depth, s.b.RootLease, true
	}
	return 0, "", false
}

// ErrSubtaskCap is returned by Reserve past the per-request spawning cap.
var ErrSubtaskCap = fmt.Errorf("subtask cap reached")

// Reserve counts n new subtasks against the root parent's per-request cap
// (ADR-0150 D4.5). It refuses, counting nothing, when they would pass max.
func (r *Subtasks) Reserve(root LeaseID, n, max int) error {
	if r == nil {
		return ErrSubtaskCap
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.parents[string(root)]
	if !ok {
		return fmt.Errorf("lease is not a subtask parent")
	}
	if max > 0 && p.spawned+n > max {
		return fmt.Errorf("%w: %d of %d subtasks already spawned for this request", ErrSubtaskCap, p.spawned, max)
	}
	p.spawned += n
	return nil
}

// AllowsLease implements LeaseToolCeiling: a child lease may use only its tools.
func (r *Subtasks) AllowsLease(lease, tool string) (inside, bound bool) {
	if r == nil || lease == "" {
		return false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.children[lease]
	if !ok {
		return false, false
	}
	return s.tools[tool], true
}

// EffectRefusal is the ToolExecutor's effect-ceiling check for a call on lease:
// "" when the lease is not a child lease or the tool is inside its ceiling,
// else the refusal the caller sees.
func (r *Subtasks) EffectRefusal(lease LeaseID, tool SystemTool) string {
	b, ok := r.Binding(lease)
	if !ok {
		return ""
	}
	beyond := EffectsBeyond(tool, b.AllowedEffects)
	if len(beyond) == 0 {
		return ""
	}
	names := make([]string, len(beyond))
	for i, e := range beyond {
		names[i] = string(e)
	}
	why := fmt.Sprintf("subtask effect ceiling: %s needs %s, but a subtask may only %s (ADR-0150 D2). Report what needs doing in your answer; the main agent does it.",
		tool.Name, strings.Join(names, "+"), effectList(b.AllowedEffects))
	if tool.EffectsInferred {
		why += " (this tool's effects are inferred from its manifest, so its egress counts as send until the manifest declares egress_read)"
	}
	return why
}

func effectList(es []ToolEffect) string {
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = string(e)
	}
	return strings.Join(names, ", ")
}

// ObserveCall records, kernel-side, that the child lease ran tool (dispatched,
// past every check). A tool with any egress marks the subtask as having read the
// web: its summary reaches the parent marked untrusted.
func (r *Subtasks) ObserveCall(lease LeaseID, tool SystemTool) {
	if r == nil || lease == "" {
		return
	}
	web := false
	for _, e := range ExpandEffects(tool.Effects) {
		if e == EffectEgressRead || e == EffectEgressSend {
			web = true
		}
	}
	if !web {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.children[string(lease)]; ok {
		s.readWeb = true
	}
}

// AddEvidence records a ledger entry a child lease's call produced.
func (r *Subtasks) AddEvidence(lease LeaseID, ledgerID string) {
	if r == nil || lease == "" || ledgerID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.children[string(lease)]; ok {
		s.evidence = append(s.evidence, ledgerID)
	}
}

// Report returns what the kernel observed of a child lease: its tool results'
// ledger ids and whether it read the web.
func (r *Subtasks) Report(lease LeaseID) (evidence []string, readWeb bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.children[string(lease)]
	if !ok {
		return nil, false
	}
	return append([]string(nil), s.evidence...), s.readWeb
}

// CallKey is the deterministic idempotency key of a call on a child lease: the
// subtask id, the tool, its canonical arguments and the occurrence of that same
// call within the subtask (the first identical call is 1, the second 2, ...).
// The subtask id is the same on a resume, and a resumed worker re-issues its
// calls, so a call that already ran answers from the stored result. ok=false for
// any other lease.
//
// The occurrence of the identical call stands in for ADR-0150 D2's "call index":
// a re-run model need not issue its calls in the same global order, but it issues
// the same call the same number of times.
func (r *Subtasks) CallKey(lease LeaseID, tool string, args []byte) (string, bool) {
	if r == nil || lease == "" {
		return "", false
	}
	canon := string(CanonicalJSON(args))
	r.mu.Lock()
	s, ok := r.children[string(lease)]
	if !ok {
		r.mu.Unlock()
		return "", false
	}
	sig := tool + "\x00" + canon
	s.calls[sig]++
	n := s.calls[sig]
	id := s.b.SubtaskID
	r.mu.Unlock()
	h := sha256.New()
	for _, p := range []string{"subtask", id, tool, canon, strconv.Itoa(n)} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// Idempotency is the store of subtask side-effecting results (CallKey) and of
// finished subtasks' results (SubtaskResultKey): durable when the registry was
// built with the ADR-0154 action record.
func (r *Subtasks) Idempotency() *IdempotencyCache {
	if r == nil {
		return nil
	}
	return r.idem
}

// SubtaskResultKey is the deterministic key (AGENTS.md §2.6) under which a
// FINISHED subtask's result is kept in the registry's idempotency store — the
// ADR-0154 durable action record, beside the subtask's write keys (CallKey) —
// so a resumed subtask, even after a kernel restart, is answered from its
// result instead of running again (ENG-1760). Never equal to a CallKey (a
// different first part).
func SubtaskResultKey(subtaskID string) string {
	h := sha256.New()
	for _, p := range []string{"subtask-result", subtaskID} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SubtaskIDFor is the deterministic id of a subtask (AGENTS.md §2.6): the parent
// run, the parent's call (its canonical arguments, standing in for the call
// index: an identical retried call is the same call) and the canonical brief.
func SubtaskIDFor(parentRun string, parentLease LeaseID, callArgs, brief []byte) string {
	h := sha256.New()
	scope := parentRun
	if scope == "" {
		scope = "lease:" + string(parentLease)
	}
	for _, p := range []string{scope, string(CanonicalJSON(callArgs)), string(CanonicalJSON(brief))} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sub-" + hex.EncodeToString(h.Sum(nil))[:24]
}

// LedgerSubtask is the ledger kind of a subtask record (brief, composition,
// status, result ref), derived from the parent's run (ADR-0150 D2 Trace).
const LedgerSubtask LedgerKind = "subtask"

// SubtaskOrigin carries, on a subtask worker's dispatch context, which subtask
// it runs (kernel-side, for the trace and logs).
type subtaskOriginKey struct{}

// WithSubtaskOrigin marks ctx as running subtask id (label) of parentRun.
func WithSubtaskOrigin(ctx context.Context, b SubtaskBinding) context.Context {
	return context.WithValue(ctx, subtaskOriginKey{}, b)
}

// SubtaskOriginFrom returns the subtask ctx runs, if any.
func SubtaskOriginFrom(ctx context.Context) (SubtaskBinding, bool) {
	b, ok := ctx.Value(subtaskOriginKey{}).(SubtaskBinding)
	return b, ok
}

// SubtaskApprovalNotice is the line an approval raised inside a subtask carries
// to the person (ADR-0150 D5.1): which subtask asks, and that only it waits.
func SubtaskApprovalNotice(label string) string {
	return fmt.Sprintf("Raised by subtask %q of the assistant's current request; only that subtask waits for your answer.", label)
}

// SubtaskRunner runs ONE subtask on a fresh worker (ADR-0150 D2): it mints a
// child lease bound to the parent's session, run and conversation owner,
// registers it in Subtasks with the binding, binds the worker's composition
// (tools, skills, brief, inputs), dispatches the composed runtime on a dedicated
// instance with no parent transcript, and returns what the worker answered and
// what the kernel observed. Implemented by the agent-plane server
// (internal/substrate/network/subtasks.go).
type SubtaskRunner interface {
	RunSubtask(ctx context.Context, d SubtaskDispatch) (SubtaskOutcome, error)
}

// SubtaskDispatch is one subtask the run_subtasks tool hands to the runner.
type SubtaskDispatch struct {
	ParentAgentID string
	// Binding is the child lease's binding (Subtasks.Bind); its SubtaskID, Label,
	// ParentLease, RootLease, Depth, AllowedEffects and Tools are final.
	Binding SubtaskBinding
	// Task is the brief rendered for the worker (kernel-written; the brief's
	// fields are data inside it).
	Task string
	// Inputs are ledger refs the worker may read (validated readable by the parent).
	Inputs []string
	// Skills the brief named (the composition's skill index; the kernel bounds it).
	Skills []string
	// MaxToolRounds is the worker's runaway cap (execution.lane.subtasks.worker_max_tool_rounds).
	MaxToolRounds int
	// MaxWallSeconds is the worker's wall-time cap
	// (execution.lane.subtasks.worker_max_wall_seconds), stamped with the lane's
	// D4 stopping rule: at it the worker makes ONE tool-less summary call and
	// answers partial (exit_reason wall), so a capped subtask returns what it
	// found (owner 2026-11-18, ENG-1760). 0 ⇒ not stamped.
	MaxWallSeconds int
}

// SubtaskOutcome is what a subtask worker returned.
type SubtaskOutcome struct {
	// Text is the worker's answer (the response payload).
	Text string
	// PayloadType is the response payload's type ("" / "text" for an answer,
	// "error", CapabilityMissingResultType, ...).
	PayloadType string
	// Meta is the response metadata.
	Meta map[string]string
	// Evidence are the ledger ids of the worker's tool results; ReadWeb whether it
	// ran any egress tool (Subtasks.Report, kernel-observed).
	Evidence []string
	ReadWeb  bool
}

// ErrSubtaskUnavailable is the runner's refusal when no subtask can run in this
// deployment (no composed runtime, no gateway): reported as status refused.
var ErrSubtaskUnavailable = fmt.Errorf("subtasks are not available in this deployment")
