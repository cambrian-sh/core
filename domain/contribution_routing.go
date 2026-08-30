package domain

import (
	"context"
	"sort"
	"strconv"
)

// WorkerTargeting is the contribution lane's ROUTING/TARGETING layer (ADR-0127
// D9, slice CL-3): the one place that answers "which of the beneficiary's
// machines serves this capability".
//
// It exists because the D1 invariant has to be provable at SELECTION and not
// only at attachment. CL-0 made the menu structurally owner-scoped (there is
// no global list of contributed tools to filter) and made dispatch re-check
// ownership. This is the third, independent layer: nothing may be TARGETED at
// a machine the task's beneficiary does not own, and that restriction sits at
// this type's entry filter AND at its single exit — so a fleet source that
// leaks a foreign worker into LiveFleet (a broken attachment layer) still
// cannot get a step aimed at it.
//
// It is deliberately NOT a routing engine and it does not touch the auction:
// contributed capabilities never compete as agents, no bid, merit or EFE value
// is read or written here, and nothing about agent selection changes. The
// auction picks the AGENT; this picks the MACHINE that agent's contributed
// step runs on.
//
// It is also the SINGLE copy of the D6 selection ladder. The executor's
// resolveByLadder delegates to Target rather than restating the rungs, so
// "routing must not prompt when a default exists" and "dispatch must not
// prompt when a default exists" cannot drift apart — they are one function.
type WorkerTargeting struct {
	// Fleet is the owner-scoped source (the WorkerHub in production). nil ⇒ no
	// lane: every selection refuses.
	Fleet FleetSource
	// Consent is the ladder's rung-4 "which machine?" channel. nil ⇒ the
	// ladder never asks and refuses instead (fail-closed).
	Consent ConsentController
	// Decisions receives every selection receipt and refusal. nil ⇒ dropped.
	Decisions func(AccessDecision)
}

// WorkerSelection is one targeting question.
type WorkerSelection struct {
	// Capability is what the step asked for: a real wire tool name or a
	// normalized capability tag (ManifestToolFor accepts either, real name
	// first).
	Capability string
	// AgentID is the attending agent, for the decision record and the prompt.
	AgentID string
	// TaskID correlates prompts and decisions with the task.
	TaskID string
	// ArgsJSON is the step's raw arguments, surfaced verbatim (bounded) on a
	// rung-4 prompt so the human answering knows what they are aiming.
	ArgsJSON []byte
	// RequestedName is the name to cite in refusals (the local:<capability>
	// the agent actually asked for). Empty ⇒ derived from Capability.
	RequestedName string
}

// WorkerTarget is a resolved targeting decision: the machine, and the REAL
// wire tool name on it. Selection speaks the capability vocabulary; the target
// it hands back speaks wire names, because that is what dispatch runs.
type WorkerTarget struct {
	Machine string
	// Tool is the real wire tool name on that machine.
	Tool string
	// ToolName is the namespaced menu/dispatch name, local:<machine>/<tool>.
	ToolName string
	// Capability is the normalized tag this resolved through.
	Capability string
	// Owner is the machine's owner principal — equal to the task beneficiary
	// by construction, checked twice on the way here.
	Owner PrincipalRef
	// Live is false when the target is registered but offline: the caller's
	// pre-dispatch gate parks it (D6).
	Live bool
	// Rung names which ladder rung answered, for the receipt.
	Rung string
	// Registration and Definition let the caller attach the tool without
	// re-resolving it.
	Registration WorkerRegistration
	Definition   SystemTool
}

func (t WorkerTargeting) record(dec AccessDecision) {
	if t.Decisions != nil {
		t.Decisions(dec)
	}
}

// owns is the CL-3 ownership restriction in one place: a machine may be
// targeted ONLY when its REGISTERED owner is the task's beneficiary. It reads
// OwnerOf — a fact independent of whatever LiveFleet answered — so a fleet
// source that leaks (or a menu that was bypassed) cannot widen targeting.
func (t WorkerTargeting) owns(ctx context.Context, beneficiary PrincipalRef, machine string) bool {
	if t.Fleet == nil || beneficiary.IsZero() || machine == "" {
		return false
	}
	owner, known := t.Fleet.OwnerOf(ctx, machine)
	return known && owner == beneficiary
}

// ownedFleet applies the ownership restriction to a fleet answer. onDrop is
// called for anything the source offered that the beneficiary does not own —
// which never happens when the source is correct, and is exactly the event
// worth recording when it is not.
func (t WorkerTargeting) ownedFleet(ctx context.Context, beneficiary PrincipalRef, in []WorkerRegistration, onDrop func(machine string)) []WorkerRegistration {
	out := make([]WorkerRegistration, 0, len(in))
	for _, w := range in {
		if !t.owns(ctx, beneficiary, w.Machine) {
			if onDrop != nil {
				onDrop(w.Machine)
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

// Vocabulary is the owner-scoped seam a routing/matching layer reads the
// contributed capability vocabulary through (D9). It resolves through exactly
// the resolution the menu uses — beneficiary on ctx → that owner's live fleet
// — and then applies the ownership restriction again, so a routing layer
// physically cannot see a capability outside the beneficiary's fleet. No
// beneficiary, or no fleet source, returns nothing: fail-closed.
//
// Entries are normalized and owner-tagged (ContributedCapability), sorted by
// tag then machine for determinism.
func (t WorkerTargeting) Vocabulary(ctx context.Context) []ContributedCapability {
	beneficiary := TaskBeneficiaryFromContext(ctx)
	if t.Fleet == nil || beneficiary.IsZero() {
		return nil
	}
	live := t.ownedFleet(ctx, beneficiary, t.Fleet.LiveFleet(ctx, beneficiary), nil)
	var out []ContributedCapability
	for _, w := range live {
		caps := w.Capabilities
		if len(caps) == 0 {
			// A fleet source that predates the derivation (or a hand-built
			// registration in a test) still enters the vocabulary.
			caps = NormalizeManifest(w)
		}
		out = append(out, caps...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tag != out[j].Tag {
			return out[i].Tag < out[j].Tag
		}
		return out[i].Machine < out[j].Machine
	})
	return out
}

// Target walks the D6 selection ladder and returns the machine that will serve
// the capability:
//
//	(1) an explicitly named machine never reaches here (that is dispatch's
//	    ordinary path, and it is owner-checked there);
//	(2) the sole capable live machine;
//	(3) the owner's configured default machine (parked if offline) — so
//	    routing does NOT prompt when a default exists;
//	(4) otherwise ASK through the initiating conversation surface.
//
// It never guesses: ending without an answer refuses with a recorded decision,
// so an effectful step can never dispatch on a guess. ok=false returns the
// caller-facing deny reason.
func (t WorkerTargeting) Target(ctx context.Context, sel WorkerSelection) (WorkerTarget, string, bool) {
	capability := sel.Capability
	requested := sel.RequestedName
	if requested == "" {
		requested = LocalToolPrefix + capability
	}
	refuse := func(reason DecisionReason, detail string) (WorkerTarget, string, bool) {
		t.record(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: requested},
			Principal: AgentPrincipal(sel.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    reason,
			Detail:    detail,
		})
		return WorkerTarget{}, "no machine could be resolved for capability " + strconv.Quote(capability) + ": " + detail, false
	}
	beneficiary := TaskBeneficiaryFromContext(ctx)
	if t.Fleet == nil || beneficiary.IsZero() || capability == "" {
		return refuse(ReasonWorkerUnresolved, "no beneficiary fleet to resolve against")
	}
	// The ownership restriction, first application: anything the source offers
	// outside the beneficiary's fleet is dropped before it can be considered,
	// and the drop is recorded — a leaking source is a defect worth seeing.
	// The record names the machine only, never whose fleet it IS in, so a
	// caller cannot probe ownership.
	dropped := func(machine string) {
		t.record(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: ContributedToolName(machine, capability)},
			Principal: AgentPrincipal(sel.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonWorkerNotOwned,
			Detail:    "machine " + strconv.Quote(machine) + " is not in the task beneficiary's fleet; refused at selection",
		})
	}

	// resolved is the SINGLE exit, and the ownership restriction's second
	// application: nothing leaves this function aimed at a machine the
	// beneficiary does not own, whichever rung chose it.
	resolved := func(reg WorkerRegistration, live bool, rung string) (WorkerTarget, string, bool) {
		if !t.owns(ctx, beneficiary, reg.Machine) {
			dropped(reg.Machine)
			return WorkerTarget{}, "worker not owned: machine " + strconv.Quote(reg.Machine) +
				" is not in the task beneficiary's fleet", false
		}
		def, found, _ := ManifestToolFor(reg, capability)
		if !found {
			return refuse(ReasonWorkerUnresolved, "machine "+strconv.Quote(reg.Machine)+" no longer offers "+strconv.Quote(capability))
		}
		name := ContributedToolName(reg.Machine, def.Name)
		t.record(AccessDecision{
			Allowed:   true,
			Resource:  ResourceRef{Kind: KindTool, ID: name},
			Principal: AgentPrincipal(sel.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonMachineSelected,
			Detail:    "machine " + strconv.Quote(reg.Machine) + " selected: " + rung,
		})
		return WorkerTarget{
			Machine: reg.Machine, Tool: def.Name, ToolName: name,
			Capability: NormalizeCapability(capability), Owner: reg.Owner,
			Live: live, Rung: rung, Registration: reg, Definition: def,
		}, "", true
	}

	live := t.ownedFleet(ctx, beneficiary, t.Fleet.LiveFleet(ctx, beneficiary), dropped)
	var liveCapable []WorkerRegistration
	liveByMachine := map[string]WorkerRegistration{}
	for _, w := range live {
		_, found, ambiguous := ManifestToolFor(w, capability)
		if ambiguous {
			// A capability tag that collapses two DIFFERENT real tools on one
			// machine resolves nothing — the lane never picks one of two. Both
			// real names still dispatch exactly (ManifestToolFor matches the
			// wire name first).
			return refuse(ReasonWorkerUnresolved, strconv.Quote(capability)+" names more than one tool on machine "+
				strconv.Quote(w.Machine)+"; call the tool by its exact name")
		}
		if !found {
			continue
		}
		liveCapable = append(liveCapable, w)
		liveByMachine[w.Machine] = w
	}
	// Rung 2: exactly one capable live machine — deterministic, not a guess,
	// and NO prompt.
	if len(liveCapable) == 1 {
		return resolved(liveCapable[0], true, "sole capable live machine")
	}
	// Rung 3: the owner's configured default machine — exactly one claimant
	// offering the capability. An offline default parks when the fleet can
	// wait; two claimants are an ambiguity and fall through to the ask.
	var defaults []WorkerRegistration
	if wr, hasReg := t.Fleet.(WorkerRegistry); hasReg {
		for _, w := range t.ownedFleet(ctx, beneficiary, wr.RegisteredFleet(ctx, beneficiary), dropped) {
			if !w.Default {
				continue
			}
			if _, offers := manifestTool(w, capability); offers {
				defaults = append(defaults, w)
			}
		}
	} else {
		for _, w := range liveCapable {
			if w.Default {
				defaults = append(defaults, w)
			}
		}
	}
	if len(defaults) == 1 {
		w := defaults[0]
		if _, isLive := liveByMachine[w.Machine]; isLive {
			return resolved(w, true, "configured default machine")
		}
		if _, canWait := t.Fleet.(LivenessWaiter); canWait {
			return resolved(w, false, "configured default machine (offline; parking)")
		}
		// A default that cannot be waited for answers nothing; fall through.
	}
	// Rung 4: ask through the initiating conversation surface — or refuse.
	if len(liveCapable) == 0 {
		return refuse(ReasonWorkerUnresolved, "no live machine offers it")
	}
	if t.Consent == nil {
		return refuse(ReasonWorkerUnresolved, "machine ambiguous and no prompt channel is wired (fail-closed)")
	}
	candidates := make([]string, 0, len(liveCapable))
	for _, w := range liveCapable {
		candidates = append(candidates, w.Machine)
	}
	sort.Strings(candidates)
	ans, outcome, err := t.Consent.Request(ctx, ConsentPrompt{
		Kind:           ConsentPromptChooseMachine,
		Candidates:     candidates,
		Tool:           capability,
		Object:         objectOfArgs(sel.ArgsJSON),
		ArgsJSON:       preview(sel.ArgsJSON, consentArgsPreview),
		TaskID:         sel.TaskID,
		AgentID:        sel.AgentID,
		Beneficiary:    beneficiary,
		ConversationID: ConversationIDFromContext(ctx),
	})
	switch {
	case err != nil:
		return refuse(ReasonWorkerUnresolved, `"which machine?" aborted: `+err.Error())
	case outcome == ConsentNoSubscriber:
		return refuse(ReasonWorkerUnresolved, `"which machine?" had no surface to route to (fail-closed)`)
	case outcome == ConsentTimedOut:
		return refuse(ReasonWorkerUnresolved, `"which machine?" went unanswered (fail-closed)`)
	case !ans.Approved:
		return refuse(ReasonWorkerUnresolved, `"which machine?" was declined`)
	}
	w, isCandidate := liveByMachine[ans.Machine]
	if !isCandidate {
		return refuse(ReasonWorkerUnresolved, "the answer named "+strconv.Quote(ans.Machine)+", which is not a live candidate")
	}
	return resolved(w, true, "chosen through the conversation surface by "+ans.AnsweredBy)
}
