package domain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// The contribution lane (ADR-0127, slice CL-0): a consumer's machines run MCP
// servers; a broker on each machine offers those tools to Cambrian over an
// OUTBOUND connection; the agents attending that consumer's tasks see them in
// their menus as local:<machine>/<tool>. This file is the kernel-side contract
// — the fleet model, the owner-scoped attachment source, and the naming — not
// the transport (poll_step/report_step and the relay are CL-1).
//
// The one sentence the whole lane is gated on: a foreign capability is
// attached to exactly the principal that supplied it — authority owner equals
// task beneficiary, per call, STRUCTURALLY. Which is why there is no global
// list of contributed tools anywhere in this file: menu resolution starts from
// the task's beneficiary owner principal and asks the fleet for that owner's
// live workers, so a forgotten filter cannot leak one owner's filesystem into
// another owner's menu — there is nothing to filter and nothing to forget.
//
// Vocabulary (ADR-0126 Context; the naming hazard is real): an "MCP server" in
// this repo means what the OUTBOUND connector dials. The thing that registers
// here is a WORKER — a named machine, owned by an owner principal, forming
// that principal's FLEET. The owner principal is what the identity plane
// returns (IdentityBinding.BoundToID, kinds BindPrincipal/BindGroup); it is
// NOT Evidence.Parties, which names who a record is ABOUT.

// LocalToolPrefix namespaces every contributed tool: local:<machine>/<tool>.
// The prefix plus the machine segment make every name unique, so two machines'
// same-named tools cannot shadow each other or a kernel tool (ADR-0127 D4).
// The registry refuses the namespace at its registration chokepoint, so the
// two resolution paths can never answer for the same name.
const LocalToolPrefix = "local:"

// WorkerConsent is the per-machine consent knob (ADR-0127 D7). Stored with the
// registration from CL-0 so the contract is complete; ENFORCED in CL-2 — no
// step relays before CL-1 ships, so nothing consults it yet.
type WorkerConsent string

const (
	// ConsentAuto runs steps without a prompt (the proposed read-only default).
	ConsentAuto WorkerConsent = "auto"
	// ConsentAnySurface routes an approval prompt to the initiating
	// conversation surface (the proposed effectful default).
	ConsentAnySurface WorkerConsent = "any-surface"
	// ConsentOnMachineOnly has the broker prompt locally — the strictest knob.
	ConsentOnMachineOnly WorkerConsent = "on-machine-only"
)

// WorkerRegistration is one machine in an owner principal's fleet.
type WorkerRegistration struct {
	// Machine is the worker's name (the machine:<name> principal id), bound at
	// token issuance: `cambrian mcp token create <machine> --owner <owner>`.
	Machine string
	// Owner is the owner principal the machine's credential was issued for.
	// It is the fleet key and the load-bearing half of the D1 invariant: these
	// tools attach ONLY to tasks whose beneficiary is this principal.
	Owner PrincipalRef
	// Tools is the worker's manifest — BARE tool definitions as the local MCP
	// servers advertise them, un-namespaced. In CL-0 this is a placeholder the
	// tests populate; CL-1's broker derives it from tools/list. Namespacing,
	// the locality note and the unconditional egress stamp happen at
	// attachment (AttachContributedTool), never in the manifest, so a worker
	// cannot lie its way out of any of them.
	Tools []SystemTool
	// Consent is the machine's D7 knob default. Recorded from CL-0, enforced
	// in CL-2.
	Consent WorkerConsent
	// Capabilities is the DERIVED ROUTE-03 capability vocabulary for this
	// registration (ADR-0127 D9, CL-3): every manifest tool name normalized
	// through NormalizeCapability (ADR-0067) and tagged with Owner, so a
	// contributed capability and a kernel agent capability naming the same
	// thing are one word rather than two vocabularies. It is DERIVED, never
	// trusted from the wire: the fleet sources overwrite it with
	// NormalizeManifest(reg) when a registration lands. The wire names in
	// Tools are untouched — dispatch keys on them.
	Capabilities []ContributedCapability
	// Default marks the machine the owner prefers when a step names a bare
	// capability with no machine (selection-ladder rung 3, ADR-0127 D6). It
	// rides the existing registration path — the broker asserts it on the poll
	// — so it is scoped to the owner's own fleet by construction and, like the
	// rest of the registration, self-heals across kernel restarts. Two machines
	// both claiming it is an ambiguity, which resolves by asking (rung 4),
	// never by guessing.
	Default bool
}

// FleetSource answers "the live fleet for owner X" — the attachment source the
// menu builders and the dispatch check resolve through (ADR-0127 D4/D6).
//
// In CL-0 liveness is driven by the in-memory implementation's explicit knob;
// CL-1 replaces that with the held poll (an open poll_step IS the liveness
// signal — the REC-02 lesson: a machine appears in menus only while it can
// actually serve a step).
type FleetSource interface {
	// LiveFleet returns the LIVE workers owned by owner, and nothing else — a
	// registered-but-offline machine contributes nothing to any menu. A zero
	// owner returns nil: no beneficiary means no fleet, fail-closed.
	LiveFleet(ctx context.Context, owner PrincipalRef) []WorkerRegistration
	// OwnerOf returns the REGISTERED owner of a machine, live or not. It is
	// the dispatch layer's independent scoping check (the D9 defense in depth
	// on top of D4's structural menu resolution): a crafted
	// local:<machine>/<tool> step is refused when the machine's owner is not
	// the task's beneficiary, whatever any menu said. false ⇒ unknown machine,
	// which refuses identically.
	OwnerOf(ctx context.Context, machine string) (PrincipalRef, bool)
}

// WorkerRegistry is an OPTIONAL capability of a FleetSource: registration
// facts for machines that are not live (CL-2). Parking needs the last offered
// manifest of an offline machine (to know the step is even servable), and
// ladder rung 3 needs the owner's registered fleet to find a default machine
// that happens to be offline. A FleetSource without it (InMemoryFleet) keeps
// the CL-0/CL-1 behaviour: an offline target refuses instead of parking.
type WorkerRegistry interface {
	// RegistrationOf returns a machine's last registration, live or not.
	RegistrationOf(ctx context.Context, machine string) (WorkerRegistration, bool)
	// RegisteredFleet returns ALL of owner's registered workers, live or not.
	// Zero owner ⇒ nil, fail-closed.
	RegisteredFleet(ctx context.Context, owner PrincipalRef) []WorkerRegistration
}

// LivenessWaiter is an OPTIONAL capability of a FleetSource: block until a
// machine is live, bounded — the D6 parking primitive. The WorkerHub
// implements it (a poll wakes parked waiters in arrival order); a FleetSource
// without it cannot park, so an offline target refuses (fail-closed, the
// CL-0/CL-1 behaviour).
type LivenessWaiter interface {
	// AwaitLive returns nil once machine is live, ErrParkExpired when wait
	// elapses first, or ctx.Err() when the caller gives up.
	AwaitLive(ctx context.Context, machine string, wait time.Duration) error
}

// Wire values for the CL-2 consent additions to the poll_step/report_step
// shapes. Both are ADDITIVE: a worker that ignores unknown step fields (the
// premium worker decodes into a struct, dropping unknowns) and never sends the
// report field is untouched.
const (
	// WireConsentOnMachine rides a dispatched step ("consent":"on-machine")
	// when the machine's knob is on-machine-only: the broker must obtain local
	// consent before executing (ADR-0127 D7).
	WireConsentOnMachine = "on-machine"
	// WireConsentDenied is the report_step consent value ("consent":"denied")
	// meaning the machine declined the step — a recorded refusal, not a worker
	// error.
	WireConsentDenied = "denied"
)

// ContributedToolName renders the namespaced menu name for one worker's tool.
func ContributedToolName(machine, tool string) string {
	return LocalToolPrefix + machine + "/" + tool
}

// SplitContributedToolName parses local:<machine>/<tool>. ok is false for
// anything else, including an empty machine or tool segment.
func SplitContributedToolName(name string) (machine, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, LocalToolPrefix)
	if !found {
		return "", "", false
	}
	machine, tool, found = strings.Cut(rest, "/")
	if !found || machine == "" || tool == "" {
		return "", "", false
	}
	return machine, tool, true
}

// AttachContributedTool is the attachment source: it turns one worker's bare
// manifest entry into the SystemTool an attending agent's menu carries.
// Everything the lane's safety depends on is stamped HERE, not trusted from
// the manifest:
//
//   - the local:<machine>/<tool> name (no shadowing, explicit targeting);
//   - a locality note prefixed to the description, so the agent can weigh
//     cost and effect domain before choosing it;
//   - the egress effect, UNCONDITIONALLY (owner ruling 2026-08-20, review
//     §4.2): a contributed call sends the agent's arguments outside the
//     deployment to the consumer's machine — even a pure read egresses, so
//     "no tool may transmit outside this network" must catch every one.
//
// Declared effects outside the closed set are dropped rather than trusted —
// the manifest is not a trust input (the A1.5 posture) — and the result is
// never empty: egress is always present, so the executor's unclassified-tool
// refusal cannot fire on a contributed entry.
func AttachContributedTool(w WorkerRegistration, t SystemTool) SystemTool {
	out := t
	out.Name = ContributedToolName(w.Machine, t.Name)
	out.Description = "Runs on the requester's machine " + w.Machine + ". " + t.Description
	effects := make([]ToolEffect, 0, len(t.Effects)+1)
	hasEgress := false
	for _, e := range t.Effects {
		if !ValidToolEffect(e) {
			continue
		}
		effects = append(effects, e)
		if e == EffectEgress {
			hasEgress = true
		}
	}
	if !hasEgress {
		effects = append(effects, EffectEgress)
	}
	sortEffects(effects)
	out.Effects = effects
	return out
}

// InMemoryFleet is the CL-0 FleetSource: registrations keyed by machine name
// (names are unique fleet-wide because they come from credential issuance,
// which is name-keyed), liveness an explicit knob. CL-1's held poll drives the
// same interface. Safe for concurrent use.
type InMemoryFleet struct {
	mu      sync.RWMutex
	workers map[string]*fleetWorker
}

type fleetWorker struct {
	reg  WorkerRegistration
	live bool
}

// NewInMemoryFleet constructs an empty fleet source.
func NewInMemoryFleet() *InMemoryFleet {
	return &InMemoryFleet{workers: map[string]*fleetWorker{}}
}

// RegisterWorker records (or replaces) one worker's registration, NOT live —
// liveness is a separate, revocable fact (SetLive; the held poll in CL-1). An
// ownerless or nameless registration is refused: a worker no beneficiary can
// ever resolve must not exist, and a zero owner would otherwise sit exactly
// where a matching zero beneficiary could reach it.
func (f *InMemoryFleet) RegisterWorker(reg WorkerRegistration) error {
	if reg.Machine == "" || reg.Owner.IsZero() {
		return errors.New("fleet: a worker registration needs a machine name and an owner principal")
	}
	// D9/CL-3: the manifest enters the capability vocabulary HERE, at the door,
	// so nothing downstream has to remember to normalize — and a wire-supplied
	// value is overwritten rather than believed.
	reg.Capabilities = NormalizeManifest(reg)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workers[reg.Machine] = &fleetWorker{reg: reg}
	return nil
}

// SetLive flips one machine's liveness. Unknown machines are a no-op — there
// is no registration to make live.
func (f *InMemoryFleet) SetLive(machine string, live bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.workers[machine]; ok {
		w.live = live
	}
}

// RemoveWorker deletes a registration (credential revoked).
func (f *InMemoryFleet) RemoveWorker(machine string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workers, machine)
}

// LiveFleet returns the live workers owned by owner. Zero owner ⇒ nil.
func (f *InMemoryFleet) LiveFleet(_ context.Context, owner PrincipalRef) []WorkerRegistration {
	if owner.IsZero() {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []WorkerRegistration
	for _, w := range f.workers {
		if w.live && w.reg.Owner == owner {
			out = append(out, w.reg)
		}
	}
	return out
}

// OwnerOf returns a machine's registered owner, live or not.
func (f *InMemoryFleet) OwnerOf(_ context.Context, machine string) (PrincipalRef, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if w, ok := f.workers[machine]; ok {
		return w.reg.Owner, true
	}
	return PrincipalRef{}, false
}

// taskBeneficiaryCtxKey carries the task's beneficiary owner principal — the
// principal whose live fleet may serve this task. Seeded by the KERNEL, never
// from a request payload (INV-5). In production the seeder is the ADR-0126
// phase-4 task lane: submit_task records the authenticated caller's owner
// principal against the task, and the agent plane re-derives it per call
// (lease → task session → the lane's index) at menu build and at tool
// dispatch. Sessions no lane submitted carry no beneficiary, so contributed
// resolution stays fail-closed for them — the chat lane threads it when the
// chat identity gap closes.
type taskBeneficiaryCtxKey struct{}

// WithTaskBeneficiary returns a child context carrying the task's beneficiary
// owner principal.
func WithTaskBeneficiary(ctx context.Context, owner PrincipalRef) context.Context {
	return context.WithValue(ctx, taskBeneficiaryCtxKey{}, owner)
}

// TaskBeneficiaryFromContext returns the beneficiary owner principal carried
// by ctx. Zero when absent — which every consumer treats fail-closed: no
// beneficiary, no fleet.
func TaskBeneficiaryFromContext(ctx context.Context) PrincipalRef {
	p, _ := ctx.Value(taskBeneficiaryCtxKey{}).(PrincipalRef)
	return p
}

// workerOwnerCtxKey carries the owner principal a machine credential was
// issued for, established by the MCP endpoint middleware when a worker
// authenticates (ADR-0127 D1: the machine principal CARRIES its owner). CL-1's
// registration path (the held poll) reads it back to key the fleet.
type workerOwnerCtxKey struct{}

// WithWorkerOwner returns a child context carrying the authenticated worker's
// owner principal.
func WithWorkerOwner(ctx context.Context, owner PrincipalRef) context.Context {
	return context.WithValue(ctx, workerOwnerCtxKey{}, owner)
}

// WorkerOwnerFromContext returns the authenticated worker's owner principal,
// zero when the caller is not a worker machine.
func WorkerOwnerFromContext(ctx context.Context) PrincipalRef {
	p, _ := ctx.Value(workerOwnerCtxKey{}).(PrincipalRef)
	return p
}

// LocalRelayStub is the CL-0 dispatch handler for contributed tools: the
// scoping layers hold (the executor refuses a cross-owner step before this
// runs), and an in-scope step gets an honest not-yet error rather than a
// silent no-op — the relay (enqueue → poll_step → report_step → D8 fencing)
// is CL-1 and does not ship half-built.
type LocalRelayStub struct{}

// Execute reports the relay as not yet implemented.
func (LocalRelayStub) Execute(context.Context, ToolCall) ([]byte, error) {
	return nil, errors.New("contribution lane: the worker relay ships in CL-1 (ADR-0127 D5); this kernel holds the CL-0 contracts only")
}
