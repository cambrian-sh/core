package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Step recovery (T-20261112-self-healer-retries-a-step-whose-yield-chain,
// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under; owner decisions
// 2026-11-12; research note 2026-11-12-agent-harness-recovery-practices §1–§2).
//
// Every step failure is classified, and the class decides what happens next:
//
//	transient      timeouts, unavailable, rate limits      → retry the same input
//	deterministic  sub-goal/depth limits, repeated intent,  → never retry
//	               validation, denied
//	needs_replan   capability missing, repeated identical   → straight to the re-planner
//	               tool failure                               with the error
//
// An error says its own class by implementing ClassifiedStepError; the self-healer
// (internal/substrate/harness) adds the transport's view (gRPC codes) for errors
// that carry none. Precedent: Temporal's non-retryable error types, LangGraph's
// retry_on defaults, the OpenAI Agents SDK's terminal MaxTurnsExceeded.

// StepFailureClass is what the step-recovery ladder does with a failure.
type StepFailureClass string

const (
	// StepFailureUnclassified: the error names no class (an agent's own error, an
	// unknown transport fault). The self-healer keeps its pre-classification
	// behaviour for it: retry, with the error handed to the next attempt.
	StepFailureUnclassified StepFailureClass = ""
	// StepFailureTransient: the same input may succeed on a retry (a timeout, an
	// unavailable or overloaded service, a rate limit).
	StepFailureTransient StepFailureClass = "transient"
	// StepFailureDeterministic: the same input fails the same way every time (a
	// budget or depth limit, a repeated intent, an invalid request, a refusal).
	// It is never retried.
	StepFailureDeterministic StepFailureClass = "deterministic"
	// StepFailureNeedsReplan: the step as planned cannot be done by the worker it
	// was given (a capability the composition lacks, a tool call that keeps
	// failing identically). It is never retried and skips the runner-up fallback:
	// the re-planner re-plans the step with the error.
	StepFailureNeedsReplan StepFailureClass = "needs_replan"
)

// ClassifiedStepError is an error that knows its step-failure class.
type ClassifiedStepError interface {
	error
	StepFailureClass() StepFailureClass
}

// classifiedError is a sentinel-friendly error with a fixed class: compare it with
// errors.Is like any errors.New sentinel.
type classifiedError struct {
	msg   string
	class StepFailureClass
}

func (e *classifiedError) Error() string                      { return e.msg }
func (e *classifiedError) StepFailureClass() StepFailureClass { return e.class }

// NewDeterministicError is errors.New for a failure that is the same on every
// attempt (a sentinel the step-recovery ladder never retries).
func NewDeterministicError(msg string) error {
	return &classifiedError{msg: msg, class: StepFailureDeterministic}
}

// ClassOfStepError returns the class an error carries: the first
// ClassifiedStepError in its chain, else the kernel's typed refusals, else a
// context deadline (transient). Unclassified otherwise; the transport's view is
// the self-healer's to add.
func ClassOfStepError(err error) StepFailureClass {
	if err == nil {
		return StepFailureUnclassified
	}
	var c ClassifiedStepError
	if errors.As(err, &c) {
		return c.StepFailureClass()
	}
	var noEffect *NoEffectMatchError
	var noCap *NoCapabilityMatchError
	if errors.As(err, &noEffect) || errors.As(err, &noCap) {
		return StepFailureDeterministic
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StepFailureTransient
	}
	return StepFailureUnclassified
}

// ── capability_missing ──────────────────────────────────────────────────────

// The capability_missing wire contract: a worker that cannot do its step because
// the tool it needs is not in its composition ENDS the step with this outcome
// instead of yielding the work to a delegate that has the same tools. It is the
// yield's sibling on the same return path (the final handoff's payload type and
// metadata; no proto change):
//
//	payload.type = "capability_missing"
//	metadata {_capability_missing: "true",
//	          _capability_needed:  what the step needs that the worker lacks — the
//	                               action or effect in words ("run a PowerShell
//	                               command", "fetch a web page"), required,
//	          _capability_tool:    optional, comma-separated tool names find_tools
//	                               reported as existing outside this step,
//	          _capability_tried:   optional, what the worker tried (short)}
//
// The kernel turns it into a CapabilityMissingError (needs_replan): the re-planner
// re-plans the step and the kernel composes the new step's worker from the
// principal's grants — the worker never widens its own tools.
const (
	CapabilityMissingResultType = "capability_missing"
	HandoffKeyCapabilityMissing = "_capability_missing"
	HandoffKeyCapabilityNeeded  = "_capability_needed"
	HandoffKeyCapabilityTool    = "_capability_tool"
	HandoffKeyCapabilityTried   = "_capability_tried"
)

// IsCapabilityMissing reports whether a worker's response is the
// capability_missing outcome rather than an answer.
func IsCapabilityMissing(h *Handoff) bool {
	if h == nil {
		return false
	}
	if h.Context != nil && h.Context[HandoffKeyCapabilityMissing] == "true" {
		return true
	}
	return h.Payload != nil && h.Payload.Type == CapabilityMissingResultType
}

// CapabilityMissingSource says who decided the step lacks a capability.
const (
	// CapabilitySourceWorker: the worker reported it (the capability_missing outcome).
	CapabilitySourceWorker = "worker"
	// CapabilitySourceSubgoal: the kernel, when a worker yielded a sub-goal whose
	// capability hint its own composition does not cover.
	CapabilitySourceSubgoal = "subgoal"
)

// CapabilityMissingError ends a step whose worker lacks a capability the step
// needs. Class needs_replan: retrying the step re-runs the same composition, and a
// sub-goal on the same lease runs under the same composition too (ADR-0145 D12);
// only a re-planned step is composed afresh.
type CapabilityMissingError struct {
	TaskID string
	// Needed is what the step needs and the worker lacks: the worker's words, or
	// the sub-goal's capability hint.
	Needed []string
	// Intent is the action the worker could not perform (a yielded sub-goal's
	// intent; "" when the worker reported the step as a whole).
	Intent string
	// OutsideStep are tools that exist within the person's access but are not in
	// this step (find_tools reported them; they were not loaded).
	OutsideStep []string
	// Tried is the worker's short account of what it tried.
	Tried string
	// Composition is the id of the composition that lacked it ("" when unknown).
	Composition string
	// Source is CapabilitySourceWorker or CapabilitySourceSubgoal.
	Source string
}

// Error is worded for the re-planner (replan_cap: "an agent could not perform an
// action for lack of a tool ... that step under-declared its effects").
func (e *CapabilityMissingError) Error() string {
	var b strings.Builder
	b.WriteString("capability missing: the worker could not perform ")
	if e.Intent != "" {
		fmt.Fprintf(&b, "%q", e.Intent)
	} else {
		b.WriteString("the step")
	}
	b.WriteString(" for lack of a tool")
	if len(e.Needed) > 0 {
		fmt.Fprintf(&b, " (needed: %s)", strings.Join(e.Needed, "; "))
	}
	if len(e.OutsideStep) > 0 {
		fmt.Fprintf(&b, "; tools that exist but are not in this step: %s", strings.Join(e.OutsideStep, ", "))
	}
	if e.Tried != "" {
		fmt.Fprintf(&b, "; tried: %s", e.Tried)
	}
	b.WriteString(". The step under-declared what it needs: re-plan it so its worker is given that capability")
	return b.String()
}

// StepFailureClass implements ClassifiedStepError.
func (e *CapabilityMissingError) StepFailureClass() StepFailureClass {
	return StepFailureNeedsReplan
}

// CapabilityMissingFromHandoff reads the capability_missing outcome off a worker's
// response (nil when it is not one).
func CapabilityMissingFromHandoff(taskID string, h *Handoff) *CapabilityMissingError {
	if !IsCapabilityMissing(h) {
		return nil
	}
	e := &CapabilityMissingError{TaskID: taskID, Source: CapabilitySourceWorker}
	if h.Context != nil {
		if n := strings.TrimSpace(h.Context[HandoffKeyCapabilityNeeded]); n != "" {
			e.Needed = []string{n}
		}
		e.OutsideStep = splitNames(h.Context[HandoffKeyCapabilityTool])
		e.Tried = strings.TrimSpace(h.Context[HandoffKeyCapabilityTried])
	}
	if len(e.Needed) == 0 && h.Payload != nil {
		// A worker that put its account in the payload instead of the key.
		if t := strings.TrimSpace(string(h.Payload.Data)); t != "" {
			e.Needed = []string{t}
		}
	}
	return e
}

func splitNames(v string) []string {
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ── repeated identical tool failure ─────────────────────────────────────────

// StepStuckError ends a step whose worker made the same tool call (tool and
// arguments) and got the same error Count times (StepFailures; OpenHands' stuck
// detector, SWE-agent's recovery odds after repeated failed edits). Class
// needs_replan: the error goes to the re-planner, which plans that work another way.
type StepStuckError struct {
	Tool        string
	ArgsPreview string
	Err         string
	Count       int
}

func (e *StepStuckError) Error() string {
	return fmt.Sprintf("step stopped: tool %s failed %d times with the same arguments (%s) and the same error: %s",
		e.Tool, e.Count, e.ArgsPreview, e.Err)
}

// StepFailureClass implements ClassifiedStepError.
func (e *StepStuckError) StepFailureClass() StepFailureClass { return StepFailureNeedsReplan }
