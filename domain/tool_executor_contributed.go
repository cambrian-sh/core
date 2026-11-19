package domain

// The ADR-0127 contribution lane on the executor: resolving local:<machine>/<tool>
// and bare capabilities against the beneficiary's fleet, and the pre-dispatch
// gate (parking, consent) — split from tool_executor.go, ENG-1531.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// contributedTools resolves the task beneficiary's live fleet into menu
// entries (ADR-0127 D4). The resolution is STRUCTURAL: it starts from the
// beneficiary carried on ctx and asks the fleet for that owner's live workers
// — there is no global list of contributed tools anywhere to filter, so a
// forgotten filter cannot put one owner's filesystem in another owner's menu.
// No beneficiary, or no fleet source, resolves to nothing, fail-closed.
//
// Grant discipline mirrors grantFor so the advisory menu keeps matching what
// Execute will allow: under the Unrestricted bypass every resolved tool
// appears; otherwise only those the agent holds a grant for under the
// namespaced name. And a name the kernel registry already holds is skipped —
// a contributed tool can never displace a kernel tool (the registry refuses
// local: names at its chokepoint, so this is a second lock on a door that is
// already shut).
func (e *ToolExecutor) contributedTools(ctx context.Context, agentID string) []SystemTool {
	if e.Fleet == nil {
		return nil
	}
	owner := TaskBeneficiaryFromContext(ctx)
	if owner.IsZero() {
		return nil
	}
	var grants []ToolGrant
	if !e.unrestricted {
		var err error
		grants, err = e.Grants.GrantsFor(ctx, agentID)
		if err != nil {
			return nil
		}
	}
	// covered mirrors grantFor's grant loop (CL-4: one matcher, two callers).
	covered := func(name string) bool {
		if e.unrestricted {
			return true
		}
		for _, g := range grants {
			if grantCovers(g, name) {
				return true
			}
		}
		return false
	}
	var out []SystemTool
	for _, w := range e.Fleet.LiveFleet(ctx, owner) {
		for _, t := range w.Tools {
			attached := AttachContributedTool(w, t)
			if _, shadow := e.Registry.Get(attached.Name); shadow {
				continue
			}
			if !covered(attached.Name) {
				continue
			}
			out = append(out, attached)
		}
	}
	return out
}

// contributedResolution is one resolved contributed step: the attached tool
// (its Name is the full local:<machine>/<tool> the rest of Execute uses), the
// worker's registration (the D7 consent knob rides it), and whether the
// machine is live right now — false means the pre-dispatch gate PARKS the step
// (ADR-0127 D6, CL-2).
type contributedResolution struct {
	tool    SystemTool
	reg     WorkerRegistration
	machine string
	bare    string
	live    bool
}

// bareContributedCapability recognises local:<capability> — a contributed call
// naming a capability but NO machine, which the selection ladder resolves.
func bareContributedCapability(name string) (string, bool) {
	rest, found := strings.CutPrefix(name, LocalToolPrefix)
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// resolveContributed is the dispatch-side resolution of a contributed call —
// and the SECOND scoping layer, independent of menu construction: the
// machine's registered owner must BE the task's beneficiary (ADR-0127 D1,
// applied again at dispatch per D9's defense in depth). A cross-owner step is
// refused with the refusal recorded as a decision; an unknown machine, an
// absent fleet and a task with no beneficiary refuse identically, so a caller
// cannot probe whose fleet a machine is in. Ownership holds for EVERY caller —
// even an operator ScopeSystem execution cannot reach a stranger's machine,
// because the authority being exercised belongs to the machine's owner, not to
// the kernel.
//
// CL-2 grows two branches on the CL-0 core:
//
//   - a name with no machine segment (local:<capability>) resolves through the
//     D6 selection ladder (resolveByLadder) — explicit naming is rung 1 and is
//     this function's ordinary path;
//   - an owned machine that is REGISTERED but not live resolves live=false
//     when the fleet can park (WorkerRegistry + LivenessWaiter — the hub);
//     the pre-dispatch gate parks it with a deadline. A fleet that cannot park
//     keeps the CL-0 refusal.
//
// ok=false returns the deny reason; refusals also flow to recordFleetDecision.
func (e *ToolExecutor) resolveContributed(ctx context.Context, req ToolCallRequest) (contributedResolution, string, bool) {
	machine, bare, wellFormed := SplitContributedToolName(req.ToolName)
	if !wellFormed {
		if capability, isBare := bareContributedCapability(req.ToolName); isBare {
			return e.resolveByLadder(ctx, req, capability)
		}
		return contributedResolution{}, "unknown tool", false
	}
	beneficiary := TaskBeneficiaryFromContext(ctx)
	owned := false
	if e.Fleet != nil && !beneficiary.IsZero() {
		if owner, known := e.Fleet.OwnerOf(ctx, machine); known && owner == beneficiary {
			owned = true
		}
	}
	if !owned {
		e.recordFleetDecision(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonWorkerNotOwned,
			Detail:    "machine " + strconv.Quote(machine) + " is not in the task beneficiary's fleet",
		})
		return contributedResolution{}, "worker not owned: machine " + strconv.Quote(machine) +
			" is not in the task beneficiary's fleet", false
	}
	// Owned — live and offering the tool is the ordinary dispatch (D6: only a
	// live worker serves a step NOW).
	for _, w := range e.Fleet.LiveFleet(ctx, beneficiary) {
		if w.Machine != machine {
			continue
		}
		t, offers, ambiguous := ManifestToolFor(w, bare)
		if ambiguous {
			// CL-3: the name arrived as a capability TAG that collapses two
			// different real tools on this machine. Never pick one of two —
			// either exact wire name still resolves outright.
			return contributedResolution{}, "contributed tool unavailable: " + strconv.Quote(bare) +
				" names more than one tool on machine " + strconv.Quote(machine) + "; call the tool by its exact name", false
		}
		if offers {
			// bare becomes the REAL wire name (an exact match is itself; a tag
			// resolves to the one tool it names), so dispatch always keys on
			// what the local server published.
			return contributedResolution{tool: AttachContributedTool(w, t), reg: w, machine: machine, bare: t.Name, live: true}, "", true
		}
	}
	// Owned but not live: PARK when the fleet supports it (CL-2, D6) — the
	// last-offered manifest must still carry the tool, and the source must be
	// able to wait for liveness. Otherwise the CL-0/CL-1 refusal stands.
	if wr, hasReg := e.Fleet.(WorkerRegistry); hasReg {
		if _, canWait := e.Fleet.(LivenessWaiter); canWait {
			if reg, known := wr.RegistrationOf(ctx, machine); known {
				if t, offers := manifestTool(reg, bare); offers {
					return contributedResolution{tool: AttachContributedTool(reg, t), reg: reg, machine: machine, bare: t.Name, live: false}, "", true
				}
			}
		}
	}
	return contributedResolution{}, "contributed tool unavailable: machine " + strconv.Quote(machine) +
		" is not live or does not offer it", false
}

// targeting builds the CL-3 owner-scoped targeting layer from this executor.
// It is the SAME object a routing/matching layer resolves through, so the D6
// ladder and the D9 ownership restriction exist exactly once.
func (e *ToolExecutor) targeting() WorkerTargeting {
	return WorkerTargeting{Fleet: e.Fleet, Consent: e.Consent, Decisions: e.recordFleetDecision}
}

// ContributedVocabulary is the seam a routing/matching layer reads the task's
// contributed capability vocabulary through (ADR-0127 D9, CL-3): normalized,
// owner-tagged, and resolved through the SAME owner-scoped resolution the menu
// uses — so routing can never see, let alone target, a capability outside the
// task beneficiary's own fleet. No beneficiary ⇒ nothing, fail-closed.
func (e *ToolExecutor) ContributedVocabulary(ctx context.Context) []ContributedCapability {
	return e.targeting().Vocabulary(ctx)
}

// resolveByLadder resolves a bare local:<capability> step by asking the
// targeting layer which machine serves it (ADR-0127 D6/D9). The rungs live in
// WorkerTargeting.Target and NOWHERE else: dispatch and routing ask the same
// function, so "prefer the default machine rather than prompting" cannot mean
// two different things in two places.
func (e *ToolExecutor) resolveByLadder(ctx context.Context, req ToolCallRequest, capability string) (contributedResolution, string, bool) {
	tgt, reason, ok := e.targeting().Target(ctx, WorkerSelection{
		Capability:    capability,
		AgentID:       req.AgentID,
		TaskID:        taskIDOf(ctx, req),
		ArgsJSON:      req.ArgsJSON,
		RequestedName: req.ToolName,
	})
	if !ok {
		return contributedResolution{}, reason, false
	}
	// bare is the REAL wire name the target resolved to, never the capability
	// tag: the broker calls its local server by the name that server published.
	return contributedResolution{
		tool:    AttachContributedTool(tgt.Registration, tgt.Definition),
		reg:     tgt.Registration,
		machine: tgt.Machine,
		bare:    tgt.Tool,
		live:    tgt.Live,
	}, "", true
}

// consentArgsPreview bounds the raw args surfaced on a prompt.
const consentArgsPreview = 500

// defaultParkDeadline bounds a parked step when mcp.worker_park_deadline_ms is
// unset: 24h — long enough for a laptop asleep overnight, bounded enough that
// the failure is seen the next day, not never.
const defaultParkDeadline = 24 * time.Hour

// parkDeadline returns the configured park window, defaulting to 24h.
func (e *ToolExecutor) parkDeadline() time.Duration {
	if e.ParkDeadline > 0 {
		return e.ParkDeadline
	}
	return defaultParkDeadline
}

// taskIDOf correlates a prompt/decision with the task: the session id the
// kernel resolved from the caller's lease, else the per-step correlation key.
func taskIDOf(ctx context.Context, req ToolCallRequest) string {
	if sid, ok := SessionIDFromContext(ctx); ok && sid != "" {
		return string(sid)
	}
	return req.TaskID
}

// objectOfArgs surfaces the exact object a step acts on where TRIVIALLY
// derivable — the raw top-level "path" (or "url") string argument, verbatim.
// Deliberately not semantic parsing: everything else stays in the prompt's raw
// ArgsJSON.
func objectOfArgs(argsJSON []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(argsJSON, &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "url"} {
		raw, present := m[k]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// contributedGate is the CL-2 pre-dispatch gate for a resolved contributed
// step: PARKING for an offline target (D6), then the CONSENT decision (D7).
// proceed=false returns the response to answer with. Every outcome — parked /
// dispatched-from-park / expired / abandoned; consent auto / approved / denied
// / timeout / unroutable / on-machine — lands on the decision seam under its
// own reason. Fail-closed throughout: no affirmative consent path, no dispatch.
func (e *ToolExecutor) contributedGate(ctx context.Context, req ToolCallRequest, c *contributedResolution, argHash string) (ToolCallResponse, bool) {
	record := func(allowed bool, reason DecisionReason, detail string) {
		e.recordFleetDecision(AccessDecision{
			Allowed:   allowed,
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    reason,
			Detail:    detail,
		})
	}

	// ── D6 parking: the target was already offline when the step arrived.
	// (A machine that drops MID-step is the hub call deadline's business —
	// CL-1 — and is deliberately not handled here.)
	if !c.live {
		waiter, canWait := e.Fleet.(LivenessWaiter)
		if !canWait {
			// Resolution only marks a step parked when the fleet can wait;
			// defensive fail-closed all the same.
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" is not live", argHash), false
		}
		wait := e.parkDeadline()
		until := time.Now().Add(wait).UTC().Format(time.RFC3339)
		record(true, ReasonStepParked, "machine "+strconv.Quote(c.machine)+" offline — queued until "+until)
		if e.Consent != nil {
			e.Consent.Notify(ctx, ConsentPrompt{
				Kind:           ConsentPromptNotice,
				Machine:        c.machine,
				Tool:           req.ToolName,
				TaskID:         taskIDOf(ctx, req),
				AgentID:        req.AgentID,
				Beneficiary:    TaskBeneficiaryFromContext(ctx),
				ConversationID: ConversationIDFromContext(ctx),
				Notice:         "machine " + c.machine + " offline — queued until " + until,
			})
		}
		switch err := waiter.AwaitLive(ctx, c.machine, wait); {
		case err == nil:
			// The machine is back — fall through to re-resolution below.
		case errors.Is(err, ErrParkExpired):
			record(false, ReasonParkExpired, "machine "+strconv.Quote(c.machine)+" stayed offline past "+wait.String())
			return ToolCallResponse{
				Error:   "parked step expired: machine " + strconv.Quote(c.machine) + " stayed offline past " + wait.String() + " (ADR-0127 D6)",
				ArgHash: argHash,
			}, false
		default:
			record(false, ReasonParkAbandoned, "caller gave up while parked: "+err.Error())
			return ToolCallResponse{
				Error:   "parked step abandoned before machine " + strconv.Quote(c.machine) + " returned: " + err.Error(),
				ArgHash: argHash,
			}, false
		}
		// Re-resolve against the FRESH manifest — the poll that woke us
		// re-registered the worker — so consent judges what will run NOW, and
		// the D1 ownership invariant is re-checked at dispatch time (a rotate
		// --owner while parked re-points the machine).
		wr, hasReg := e.Fleet.(WorkerRegistry)
		if !hasReg {
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" returned without a readable registration", argHash), false
		}
		reg, known := wr.RegistrationOf(ctx, c.machine)
		t, offers := manifestTool(reg, c.bare)
		if !known || !offers {
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" no longer offers "+strconv.Quote(c.bare), argHash), false
		}
		if reg.Owner != TaskBeneficiaryFromContext(ctx) {
			record(false, ReasonWorkerNotOwned, "machine "+strconv.Quote(c.machine)+" is not in the task beneficiary's fleet")
			return denied("worker not owned: machine "+strconv.Quote(c.machine)+" is not in the task beneficiary's fleet", argHash), false
		}
		c.reg, c.tool, c.live = reg, AttachContributedTool(reg, t), true
		record(true, ReasonParkDispatched, "machine "+strconv.Quote(c.machine)+" returned; dispatching through the consent-checked path")
	}

	// ── D7 consent: knob × effect class → outcome.
	knob := c.reg.Consent
	if knob == "" {
		knob = ConsentAuto
	}
	effectful := ContributedStepEffectful(c.tool.Effects)
	switch {
	case knob == ConsentOnMachineOnly:
		// Strictest knob: the broker prompts locally for EVERY step; the wire
		// step carries the consent marker (hub-side) and a consent-denied
		// report is a recorded refusal.
		record(true, ReasonConsentOnMachine, "broker prompts locally; a consent-denied report is a recorded refusal")
		return ToolCallResponse{}, true
	case knob == ConsentAuto && !effectful:
		// The sealed default: reads run silently but receipted.
		record(true, ReasonConsentAuto, "read-only step under the auto knob")
		return ToolCallResponse{}, true
	}
	// any-surface — or effectful under auto, which the sealed ruling treats as
	// any-surface: approval is the default for anything effectful.
	if e.Consent == nil {
		record(false, ReasonConsentUnroutable, "consent required but no consent channel is wired (fail-closed)")
		return denied("consent required for "+req.ToolName+" but no consent channel is wired (fail-closed)", argHash), false
	}
	ans, outcome, err := e.Consent.Request(ctx, ConsentPrompt{
		Kind:           ConsentPromptApprove,
		Machine:        c.machine,
		Tool:           req.ToolName,
		Object:         objectOfArgs(req.ArgsJSON),
		ArgsJSON:       preview(req.ArgsJSON, consentArgsPreview),
		TaskID:         taskIDOf(ctx, req),
		AgentID:        req.AgentID,
		Beneficiary:    TaskBeneficiaryFromContext(ctx),
		ConversationID: ConversationIDFromContext(ctx),
	})
	switch {
	case err != nil:
		record(false, ReasonConsentTimeout, "consent request aborted: "+err.Error())
		return denied("consent not obtained for "+req.ToolName+": "+err.Error(), argHash), false
	case outcome == ConsentNoSubscriber:
		record(false, ReasonConsentUnroutable, "no surface is subscribed to answer (fail-closed)")
		return denied("consent required for "+req.ToolName+" but no surface is listening (fail-closed)", argHash), false
	case outcome == ConsentTimedOut:
		record(false, ReasonConsentTimeout, "consent request went unanswered (fail-closed)")
		return denied("consent request for "+req.ToolName+" timed out (fail-closed)", argHash), false
	case !ans.Approved:
		record(false, ReasonConsentDenied, "denied by "+ans.AnsweredBy)
		return denied("consent denied for "+req.ToolName+" on machine "+strconv.Quote(c.machine), argHash), false
	}
	record(true, ReasonConsentApproved, "approved by "+ans.AnsweredBy)
	return ToolCallResponse{}, true
}

// recordFleetDecision makes a contributed-lane decision durable-adjacent:
// always logged (allows at info, refusals at warn), and handed to the
// FleetDecisions seam when one is wired (tests; the premium decision journal).
func (e *ToolExecutor) recordFleetDecision(dec AccessDecision) {
	if dec.Allowed {
		slog.Info("ADR-0127: contributed-step decision", "decision", dec.Explain())
	} else {
		slog.Warn("ADR-0127: contributed-step refused", "decision", dec.Explain())
	}
	if e.FleetDecisions != nil {
		e.FleetDecisions(dec)
	}
}
