---
number: 0096
title: Explicit Agent Pinning
status: Implemented
date: 2026-07-28
relates-to: [0002, 0003, 0007, 0037, 0067]
---

# ADR-0096: Explicit Agent Pinning

## Status

Implemented (2026-07-28).

## Context

The planner had no way to say which agent should run a step, and it needed one.

Its own prompt invited the attempt — *"You may reference a specific agent ID from
the capability clusters if the task is unambiguously domain-specific"* — while the
plan schema offered no field to put an agent ID in. Under the ROUTE-03 capability
contract the model resolved that contradiction the only way it could: it emitted

```json
{"query": "...", "required_capabilities": ["terminal_agent"]}
```

For as long as L1 did not actually enforce the capability contract this was
harmless noise. The moment enforcement worked (the EFE task-rebuild defect, fixed
the same day) the smuggled agent name became a requirement that **no agent could
declare**, every candidate was filtered, and the step died with `no candidates`.
Measured on `runs/rt_fix1`: 66 L1 filter events, 4 `no candidates`, task failed.

Separately, users want this directly: *"if user says use xxx agent to do yyy, that
agent should be prioritized."*

Both pressures point at the same missing primitive — a first-class channel for
naming an executor, distinct from the capability contract.

## Decision

### D1 — Naming an agent is DATA on a step, not routing logic in Go

`Step.PreferredAgent` carries an agent ID; `Step.AgentPin` carries its strength.
This does not weaken the Zero-Hardcode Rule. That rule forbids *authored*
agent-to-task routing tables in Go — `if task == X { use agentY }` — because such
tables encode yesterday's fleet and rot. A user saying "use terminal_agent"
is an instruction arriving at runtime as data; honouring it adds no branch on
agent identity to the kernel's routing decisions. The rule's three sanctioned
exceptions (system-shell, reflexive path, security gates) are untouched.

### D2 — Two strengths, and the weaker one is the default

**Soft** (`PinSoft`) prioritises without guaranteeing: the named agent is exempt
from the L2 semantic gate and receives `SoftPinBoost` (0.25, clamped to 1.0) on
its merit score. It still competes, and can still lose to a markedly better
candidate. An unknown or unregistered name degrades to ordinary selection.

**Hard** (`PinHard`) binds the named agent directly, returning it as the entire
candidate slate — no auction, no semantic gate. An unavailable name is
`ErrPinnedAgentUnavailable`, not a fallback.

The asymmetry is deliberate. A soft pin is an inference (the planner's, or a
user's hedge) and must never strand a step, so it fails open. A hard pin is an
explicit directive, and silently substituting a different agent for one the user
named is a worse answer than failing — so it fails closed. Anything not
recognisably `"hard"` (empty, malformed, unexpected casing is folded) resolves to
soft, so every degradation path lands on the non-stranding behaviour.

### D3 — A soft pin never buys past the capability contract

The boost is applied AFTER L1. A soft-pinned agent that cannot satisfy the step's
`required_capabilities` is filtered like any other. Priority and eligibility are
different questions, and only the second is a correctness gate.

A hard pin does skip L1, because the user has asserted the executor — but it can
never reach a daemon or a privileged system organ (ADR-0051 Scout and friends).
Those do not serve task steps at all, and a pin must not become a route into
machinery that cannot run the work.

### D4 — The pin must survive every rebuild between planner and gate

`PreferredAgent`/`AgentPin` are carried on `Step`, `AuctionTask`, **and**
`domain.Intent`, and are copied in `ExecutionPlan.Clone`.

This is not redundancy, it is the lesson of the defect that motivated this ADR.
Every selector arm rebuilds an `AuctionTask` from an `Intent` field-by-field, and
`Clone` rebuilds `Step` field-by-field; a field omitted at any of those sites
vanishes with no compiler error. That is exactly how the EFE arm silently
exempted itself from the ROUTE-03 capability contract. Any future field on these
types must be added at all of them.

### D5 — Default ON, behind a flag

`execution.agent_pinning` defaults **true**, unlike the learning arms
(ROUTE-05/06/07), which default off pending offline evidence. Those gate a model
whose value is unproven; this gates directive-following, whose value is stated by
the user in the request. It remains a flag so the routing change can be A/B'd and
killed in one place; OFF ignores both fields and restores unpinned selection
exactly, including refusing to honour a hard pin.

### D6 — The prompt names the field, and closes the leak

The instruction inviting free-form agent references is **removed**, not
supplemented, and replaced by an `AGENT PINNING` block present in both planner
arms, plus `preferred_agent`/`agent_pin` in both output schemas. The block states
the default (do not name an agent), when to pin hard vs soft, and explicitly that
an agent ID must never appear in `required_capabilities`. Leaving the old
invitation in place alongside the new field would preserve the ambiguity that
caused the failure.

## Consequences

**Positive.** An explicit user directive is honoured. The planner has a legitimate
channel for something it was already attempting, so agent names stop poisoning the
capability contract. Pin decisions are visible in the plan and at the gate
(`Gatekeeper: hard agent pin bound`).

**Negative.** A hard pin bypasses merit entirely, so a user can pin a weak agent
and get its failure — observed live: `terminal_agent`, hard-pinned for a file
write, failed one run and succeeded the next, where unpinned selection had chosen
`code_generator_agent` and succeeded. That is the semantics as designed, but it
means a hard pin transfers responsibility for the choice to whoever made it.

**Negative.** A hard pin returns a single candidate, so the inter-step fallback
loop has no runner-ups to try. A pinned step that fails has no second option
beyond replanning.

**Neutral.** `SoftPinBoost` is a constant, not a tuned parameter. It is sized to
dominate ordinary merit spread without being absolute; if measurement shows soft
pins winning or losing too often it should become config.

## Verification

- Unit: 9 tests in `internal/supervision/gatekeeper/agent_pin_test.go` covering
  hard-binds-only-named, case-insensitive strength, unknown/undispatchable errors,
  soft-ranks-first-but-keeps-others, soft-does-not-bypass-L1, empty-degrades-to-soft,
  arm-off, and boost clamping. Mutation-checked: neutering the boost fails two.
- `domain/plan_pin_test.go` guards the `Clone` silent-drop class.
- `internal/awareness/planner_capability_test.go` asserts both schemas expose the
  fields, both prompts carry the rules block, and the old invitation is gone.
- Live E2E (`runs/pin_live`): prompt "Use terminal_agent to create a file …" →
  planner emitted `"preferred_agent": "terminal_agent"` with a hard pin → kernel
  logged `hard agent pin bound agent_id=terminal_agent` → `terminal_agent`
  executed and the file landed with the exact expected content.
