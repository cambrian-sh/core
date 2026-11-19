package domain

import (
	"context"
	"errors"
	"fmt"
)

// ToolExecutorControls are the security controls of the tool reference monitor,
// handed to NewToolExecutor so they are in place before the first call
// (AGENTS.md §2.3: enforcement is constructor-injected, never attached later;
// T-20261113-domain-toolexecutor-has-no-constructor-its).
//
// Required (NewToolExecutor refuses nil): Registry, Grants and Authz. Authz has
// no safe zero value — a nil one used to mean "allow all" silently — so the OSS
// default is passed explicitly as AllowAllAuthorizer{}.
//
// The rest are config-gated controls whose zero value is a deliberate,
// config-chosen "off" (or, for Approval, fail-closed): the caller reads the flag
// and passes the control or nil here, in the one place the executor is built.
type ToolExecutorControls struct {
	Registry ToolRegistry
	Grants   GrantsProvider
	Authz    Authorizer

	// Approval: nil ⇒ dangerous tools are denied (fail-closed).
	Approval ApprovalController
	// Ceiling is the profile tool ceiling; Unrestricted the tools_unrestricted
	// bypass (see the ToolExecutor fields of the same names).
	Ceiling      ToolCeiling
	Unrestricted bool
	// The ADR-0145 D9/D14 controls (execution.tools.validate_args,
	// idempotent_writes, request_guards, write_targets); nil ⇒ off.
	ArgValidator ToolArgValidator
	Idempotency  *IdempotencyCache
	Guards       *RequestGuards
	WriteTargets *WriteTargets
	// LeaseCeiling is the ADR-0139 D12 per-lease tool ceiling (the composed
	// leases and the subtask registry, chained by the caller); nil ⇒ none.
	// Subtasks is the ADR-0150 subtask registry (effect refusal and
	// deterministic write keys for child leases); nil ⇒ no subtasks. Both used
	// to be attached after construction by app/composed_workers.go and
	// app/subtasks.go.
	LeaseCeiling LeaseToolCeiling
	Subtasks     *Subtasks
	// OutboundIdentifiers is the outbound identifier guard
	// (execution.tools.outbound_identifier_guard, default on; ENG-917): a call
	// that can send outside (egress_send) whose text arguments carry a personal
	// identifier goes to approval with the identifiers named. nil ⇒ off.
	OutboundIdentifiers *OutboundIdentifierGuard
	// UntrustedOutput is the MCP response injection scan
	// (execution.tools.output_scan, default on; ADR-0043 D4, ENG-117): every MCP
	// response is scanned with the shared content scanner and a hit is flagged or
	// withheld. nil ⇒ not scanned (MCP responses are fenced either way).
	UntrustedOutput *UntrustedOutputGuard
	// RuleOfTwo is the Rule-of-Two gate (execution.tools.rule_of_two, default
	// on; ENG-531): a write/egress call from a session that consumed untrusted
	// content and holds sensitive access goes to approval. nil ⇒ off.
	RuleOfTwo *RuleOfTwoGate
	// FreeQueryOpen lifts the free-query guard (ENG-2232; execution.tools.
	// free_query.require_explicit_grant=false). Its zero value is the guard: an
	// agent reaches a SystemTool.FreeQuery tool only through a grant naming it
	// exactly — never the Unrestricted bypass, a skill's run-scoped overlay or
	// a fleet wildcard (the LLM never writes SQL, ADR-0149 D6).
	FreeQueryOpen bool
}

// ErrToolExecutorControl reports a missing or doubly supplied security control.
var ErrToolExecutorControl = errors.New("tool executor: security control")

// NewToolExecutor builds the tool reference monitor. features carries the
// non-security wiring (handlers, content sizing, artifact promotion, hooks, …)
// as a ToolExecutor value; a security control set on it instead of on controls
// is refused, so there is exactly one way in for each control.
func NewToolExecutor(controls ToolExecutorControls, features ToolExecutor) (*ToolExecutor, error) {
	switch {
	case controls.Registry == nil:
		return nil, fmt.Errorf("%w: Registry is required", ErrToolExecutorControl)
	case controls.Grants == nil:
		return nil, fmt.Errorf("%w: Grants is required", ErrToolExecutorControl)
	case controls.Authz == nil:
		return nil, fmt.Errorf("%w: Authz is required (pass AllowAllAuthorizer{} for the OSS default explicitly)", ErrToolExecutorControl)
	}
	if features.Registry != nil || features.Grants != nil || features.authz != nil || features.approval != nil ||
		features.ceiling != nil || features.unrestricted || features.argValidator != nil ||
		features.idempotency != nil || features.guards != nil || features.writeTargets != nil ||
		features.leaseCeiling != nil || features.subtasks != nil || features.outboundIdentifiers != nil ||
		features.untrustedOutput != nil ||
		features.ruleOfTwo != nil || features.freeQueryOpen {
		return nil, fmt.Errorf("%w: set on features; pass it in ToolExecutorControls", ErrToolExecutorControl)
	}
	te := features
	te.Registry = controls.Registry
	te.Grants = controls.Grants
	te.authz = controls.Authz
	te.approval = controls.Approval
	te.ceiling = controls.Ceiling
	te.unrestricted = controls.Unrestricted
	te.argValidator = controls.ArgValidator
	te.idempotency = controls.Idempotency
	te.guards = controls.Guards
	te.writeTargets = controls.WriteTargets
	te.leaseCeiling = controls.LeaseCeiling
	te.subtasks = controls.Subtasks
	te.outboundIdentifiers = controls.OutboundIdentifiers
	te.untrustedOutput = controls.UntrustedOutput
	te.ruleOfTwo = controls.RuleOfTwo
	te.freeQueryOpen = controls.FreeQueryOpen
	return &te, nil
}

// The controls are unexported fields set only by NewToolExecutor (AGENTS.md
// §2.3): code outside domain/ can read the ones it consumes through these
// accessors, never replace them after construction.

// Guards returns the ADR-0145 D14 request guards (nil ⇒ off).
func (e *ToolExecutor) Guards() *RequestGuards {
	if e == nil {
		return nil
	}
	return e.guards
}

// WriteTargets returns the per-target write keying (nil ⇒ off).
func (e *ToolExecutor) WriteTargets() *WriteTargets {
	if e == nil {
		return nil
	}
	return e.writeTargets
}

// Idempotency returns the ADR-0145 D9 plan-step idempotency cache (nil ⇒ off).
func (e *ToolExecutor) Idempotency() *IdempotencyCache {
	if e == nil {
		return nil
	}
	return e.idempotency
}

// Subtasks returns the ADR-0150 subtask registry the executor enforces (nil ⇒ none).
func (e *ToolExecutor) Subtasks() *Subtasks {
	if e == nil {
		return nil
	}
	return e.subtasks
}

// RuleOfTwo returns the Rule-of-Two gate (nil ⇒ off).
func (e *ToolExecutor) RuleOfTwo() *RuleOfTwoGate {
	if e == nil {
		return nil
	}
	return e.ruleOfTwo
}

// ObserveContent reports content a session consumed outside a tool call — a
// memory recall (QueryMemory) — to the Rule-of-Two gate, under the taint scope
// the transport put on ctx (WithTaintScope). class is the content's source
// class, tags its classification tags (only the sensitive ones are kept). A
// no-op when the gate is off.
func (e *ToolExecutor) ObserveContent(ctx context.Context, class SourceClass, tags []string) {
	e.observe(ctx, class, tags)
}

func (e *ToolExecutor) observe(ctx context.Context, class SourceClass, tags []string) {
	if e == nil || e.ruleOfTwo == nil {
		return
	}
	e.ruleOfTwo.Observe(TaintScopeFromContext(ctx), class, tags)
}

// grantedTags returns the classification tags of every tool agentID holds a
// grant on (the Rule-of-Two sensitive-access leg). An unreadable grant store is
// an error, which the gate reads as "sensitive" (fail closed).
func (e *ToolExecutor) grantedTags(ctx context.Context, agentID string) ([]string, error) {
	if agentID == "" {
		return nil, nil
	}
	var tools []SystemTool
	if e.unrestricted {
		tools = e.Registry.All()
	} else {
		grants, err := e.Grants.GrantsFor(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("grants for %s: %w", agentID, err)
		}
		tools = e.Registry.SchemasFor(grants)
	}
	var out []string
	for _, t := range tools {
		out = append(out, t.AuthzTags()...)
	}
	return out, nil
}
