# ADR-0046: Agent Skills — Authored Procedural Capabilities, System and Agent-Local

**Status:** Proposed (2026-06-13) — design recorded via a grilling session; not implemented. Sibling to ADR-0045 (the two share the capability-retrieval requirements doc).
**Date:** 2026-06-13
**Author:** Afsin
**Depends on:** ADR-0044 (Semantic Tool Retrieval — the `ToolRetriever` port + `DocType` sibling-index pattern reused for `DocTypeSkill`), ADR-0045 (Two-Tier Tool Disclosure — the deterministic deriver + "short to choose, full to call" disclosure reused), ADR-0039 (kernel-owned tool registry — `ToolExecutor`, `GrantsProvider`, `grantFor`, approval), ADR-0034 (tag-based scope — gates which agents may load a system skill), ADR-0035 (kernel-derived authority — agents narrow, never broaden), ADR-0041 (LRW — instructions inject into the agent's typed working memory).
**Relates to:** ADR-0027 (procedural templates — the boundary drawn in D10), the SDK requirements doc `docs/requirements/agent-sdk-local-rp-step-loop.md` (CodeAct/confinement, to which bundled-script execution is deferred).

---

## Context

Cambrian has three relevance-retrieved channels — memory (facts), tools (actions), and agent profiles — but no first-class **reusable "how-to" capability**. `DocTypeProceduralTemplate` (ADR-0027) is a *learned* seed for the Planner, not an *authored*, progressively-disclosed procedure an agent can load.

A **Skill** fills that gap: an authored procedure (instructions + a tool-grant bundle) that an agent loads to steer how it executes. The governing steer from design: **skills load like tools.** That means skills reuse the tool *discovery + disclosure + scope* machinery (ADR-0044/0045/0034) and inherit the tool world's **two-loci split** — kernel-owned **system** capabilities vs an agent's **local** ones (the `@tools` analog, `python-sdk/cambrian_agent_sdk/react.py:316`). They diverge from tools only at **execution**: a tool is a leaf call returning a value; a skill is a *context + grant mutation* that changes what the agent does next.

This ADR records the skill model. It stays inside ADR-0044's no-LLM-on-the-hot-path stance, the ADR-0034/0035/0039 trust model (no new permission system), and the Zero-Hardcode Rule.

---

## Decision

### D1 — A Skill is an authored procedural capability

Domain type (in its own package — **distinct from `A2ASkill`**, the A2A AgentCard declaration in `agent_card.go:14`):

```go
type Skill struct {
    Name         string      // identity
    Description  string      // Tier-1 one-liner source (the ADR-0045 deriver)
    Instructions string      // SKILL.md body — Tier-2, injected on use_skill
    ToolGrants   []string    // bundled tools, activated run-scoped (D6)
    Scope        ScopeConfig // ADR-0034; meaningful for system skills (D9)
}
```

A skill is **authored**, not learned (D10), and **instruction-only** in v1 (D8).

### D2 — Two loci: kernel system skills, agent-local agent skills

Skills inherit the tool world's split exactly:

| | Locus | Index | Retrieval |
|---|---|---|---|
| **System skill** | kernel-owned | `DocTypeSkill` in pgvector | kernel ranks + pushes (D4) |
| **Agent skill** | **agent-local** (in the agent's SDK process) | **none — never enters the kernel index** | always present locally (D5) |

This is the `@tools` analog: the kernel never sees an agent's private capabilities. The kernel routes a task to an *agent* (via the auction/Gatekeeper on its profile); the agent then uses its private skills/tools internally. Agent skills being invisible to the kernel is consistent with the existing two-level (Global-RP kernel / Local-RP agent) model.

### D3 — `use_skill(name)`: a state-mutating load (not a call)

`use_skill` is a new ReAct action that, for the named skill:
1. **injects** its `Instructions` (the SKILL.md body) into the agent's LRW working memory for the rest of the run, and
2. **activates** its `ToolGrants` as a run-scoped overlay (D6),

then **returns control to the loop** — the agent continues its normal single-action-per-turn loop, now steered by the instructions and able to `tool_call` the granted tools. It is distinct from `describe_tool` (pure fetch, ADR-0045) and `tool_call` (leaf execution).

**"The run" = the current task invocation:** a loaded skill's instructions + grants persist across the loop's turns until that task completes, then fall away. Not one-turn, not cross-task. Keeping the agent a thin Local-RP loop (ADR-0041), `use_skill` reshapes context + capability — it does not spawn a second control structure.

`use_skill` has two delivery paths: an **agent (own) skill** loads purely locally (the SDK already holds the instructions; it injects them and asks the kernel to activate the grants); a **system skill** fetches its Tier-2 instructions from the kernel (`ListSkills(name, full=true)`) then activates grants.

### D4 — System-skill retrieval/disclosure reuses the tool pattern (sibling, not merged)

For **system skills only**:
- A sibling **`DocTypeSkill`** index — *not* a merged `Capability` DocType. (ADR-0044 already runs tools/memory/agent-profiles as sibling DocTypes sharing one pgvector index via ports; skills slot into that pattern with no new abstraction.)
- The same `ToolRetriever` **port shape**, a sibling adapter pointed at `DocTypeSkill`.
- The same ADR-0045 **deterministic deriver** for the Tier-1 one-liner, and the same **two-tier disclosure**: Tier-1 = `name + summary` in the menu (embedded for retrieval); Tier-2 = full `Instructions` on `use_skill`.
- The same **hybrid discovery**: a relevance **push** at task start + a **`find_skills(need)`** pull (the `find_tools` analog).
- A **parallel `ListSkills` RPC** mirroring `ListTools` (query/k/names/full) — *not* an overloaded `ListTools`; response shapes and actions differ, so a sibling message (`SkillDescriptor`) keeps each wire clean and leaves ADR-0045's `ToolDescriptor` untouched.
- A **distinct `[skills]` menu section** with distinct actions — an agent *calls* a tool but *loads* a skill; surfacing them as one list would blur the action choice.

### D5 — Agent skills: always-present local menu, structural prioritization

Agent skills are held by the SDK and **always listed** in the agent's local skill menu (small N, like `@tools` — no retrieval needed). System skills are **appended only when they clear the relevance floor** (D4).

This makes **agent-owned skills structurally prioritized** with no ranking hack and no Zero-Hardcode tension: the agent's own skills are *always available*; the org's appear *only when relevant*. **Same-name shadowing** is the degenerate case — an agent skill named `deploy` is in the menu and the SDK drops a system `deploy` from that agent's view. This is lexical scoping (the `@tools`/agent-scoped-memory analog), deterministic identity resolution — **not** agent-to-task routing, so it is Zero-Hardcode-legal. There is **no central agent-vs-system ranking** (the kernel never holds agent skills to rank) and **no hardcoded capability precedence** in the Planner/auction.

### D6 — Grant authority: run-scoped overlay, bounded by authoring authority

`use_skill` activates the skill's `ToolGrants` as a **run-scoped overlay** on `GrantsProvider`, resolved in `grantFor` (`tool_executor.go:259`) for the duration of the task. Authority is bounded by **who authored the skill**:
- **System skill** — operator-authored, so the operator blessing it authorizes its bundled grants: it **may confer** tools (the operator is the trust authority, as for all static grants).
- **Agent skill** — may only activate grants **intersected with the agent's existing operator envelope**: narrow-only, never broadening (the ADR-0035 analog). An agent cannot self-escalate by loading its own skill.

Three existing backstops keep even a conferring system skill bounded: **(1)** loading is **ADR-0034 scope-gated** (only permitted agents may load a system skill); **(2)** dangerous tools still hit `ApprovalController` at execute time regardless of how granted (`tool_executor.go:121`); **(3)** the overlay is **ephemeral** — it falls away when the task completes. No new permission system.

### D7 — Authoring: file-based `SKILL.md`; runtime API deferred

A skill is a `SKILL.md` with **frontmatter** (`name`, `description`, `tools: [...]`, `scope`) + a markdown **body** (the `Instructions`). Same shape as a tool manifest.
- **System skills:** discovered from a kernel `skills/` directory at boot (the `tools/` `LoadRegistry` analog), indexed as `DocTypeSkill`. Self-migrating on boot (ADR-0045 D7).
- **Agent skills:** shipped in the agent's package and loaded into the SDK at agent start (the way `@tools`/`AGENT_MANIFEST` are discovered at registration).

A **runtime per-agent skill-registration API is deferred to v2** — static declaration satisfies "both system and agent skills" without a mutable write-path/lifecycle/precedence surface; the dynamic API is a clean later add when a concrete need appears.

### D8 — Instruction-only + tool grants in v1

A v1 skill is `Instructions + ToolGrants`, nothing executable or fetched. A skill that needs **deterministic execution** expresses it by **bundling a grant to a tool** that runs it — not by carrying a script. This keeps the boundary crisp (**tool = executable leaf; skill = procedural context + grant bundle**) and keeps the deferred, confinement-gated CodeAct decision out of skills v1 (Wasm is cancelled; the only confinement primitive is the confined-Python-subprocess behind `ToolHandler`). Bundled **scripts** (confinement-gated) and bundled static **resources** (a Tier-3 fetch path) are both deferred.

### D9 — Scope: ADR-0034 for system skills; agent skills trivially local

A **system skill** carries a `scope` (frontmatter, operator-set, typically broad); the system-`SkillRetriever` applies the existing ADR-0034 scope filter (the `ScopedVectorStore`/CNF predicate used for memory), so an agent only retrieves system skills its effective scope permits. **Agent skills need no scope tagging** — they never enter the shared index; being local *is* their scope.

### D10 — Boundary vs. procedural templates (ADR-0027): distinct, authored-only

A Skill is **not** a procedural template — same word, different layer:

| Axis | Hippocampus template (ADR-0027) | Skill (this ADR) |
|---|---|---|
| Origin | **learned** (auto-captured from success) | **authored** (`SKILL.md`) |
| Altitude | **Global RP** — primes the Planner | **Local RP** — injected into one agent's loop |
| Unit | an `ExecutionPlan` (DAG of steps → agents) | NL instructions + a tool-grant bundle |
| Trigger | the Planner, automatically | the agent, via `use_skill` |
| Storage | `DocTypeProceduralTemplate` | `DocTypeSkill` |

They **compose, never collide** — the Planner may seed a plan from a template, and within one of that plan's steps an agent may load a skill. The one thing that *would* collide is **skill-learning** (auto-generating skills from agent behavior), which re-creates Hippocampus at the wrong altitude — so v1 skills are **authored-only**, and learned skills, if ever built, must be designed explicitly against this boundary.

---

## Consequences

**Positive**
- A first-class authored "how-to" capability, retrieved/disclosed/scoped with the same machinery as tools — minimal new abstraction (one sibling `DocType`, one parallel RPC, one new action).
- The two-loci split keeps agent specialization private (agent skills never burden the kernel index) while system skills get org-wide reuse — consistent with the existing kernel-routes-to-agents / agent-uses-private-capabilities model.
- Agent-owned prioritization, same-name shadowing, and "no central precedence" all fall out of the always-present-local-menu structure — Zero-Hardcode-clean, no routing in Go.
- Skill-as-capability-delegation (D6) is powerful but bounded: operator confers via system skills, agents can only narrow, dangerous tools still gate on approval, grants are ephemeral.

**Negative / costs**
- A new RPC (`ListSkills`), a new index (`DocTypeSkill`), a new action (`use_skill`/`find_skills`), and a run-scoped grant overlay on `ToolExecutor` — real surface, justified by making skills a true sibling of tools.
- Injected SKILL.md instructions are large and compete for the LRW token budget — this couples to, and motivates, the SDK dynamic-context work (sibling requirements doc).
- A loaded system skill is a grant vector; correctness depends on the three backstops (scope-gate, approval, ephemerality) holding together.

**Neutral**
- ADR-0027 procedural templates are untouched; the boundary (D10) is definitional, not a code change.

---

## Alternatives considered

- **Index agent skills centrally (one locus).** Rejected: breaks the `@tools` analog, burdens the kernel index with per-agent specialization, and forces central agent-vs-system ranking — the inconsistency that surfaced in grilling.
- **Merged `Capability` DocType with a `kind` field (vs sibling `DocTypeSkill`).** Rejected: a migration-heavy abstraction buying nothing over ADR-0044's established sibling-DocType pattern.
- **Skill grants authoritative / pure menu-curation (vs D6 authority-bounded).** Authoritative rejected (self-escalation hole); pure curation rejected (guts the point of bundling). Authority-bounded is the safe middle that still delegates real capability.
- **Skills bundle/run scripts in v1 (vs D8 instruction-only).** Rejected: forces the deferred confinement/CodeAct decision; "grant a tool that runs it" already expresses the need.
- **Hardcoded agent-shadows-system precedence in the Planner (vs D5 structural).** Rejected as agent-to-task routing in Go (Zero-Hardcode); the always-present-local-menu achieves the same prioritization structurally.
- **Skill = a kind of procedural template (vs D10 distinct).** Rejected: different altitude, origin, unit, storage, and trigger; unifying them would entangle Local-RP and Global-RP.

---

## Verification

- A system skill is discovered from `skills/`, indexed `DocTypeSkill`, retrieved by relevance push + `find_skills`, scope-filtered, and `use_skill` injects its instructions + activates grants for the run only.
- An agent skill is always present in its agent's local menu, shadows a same-named system skill for that agent, and is never visible to another agent or the kernel index.
- Grant-authority invariant: a system skill may confer a tool the agent lacked (still subject to approval); an agent skill cannot activate a grant outside the agent's operator envelope; all activations vanish at task end.
- Loading a scope-denied system skill fails closed (absence, no existence leak), mirroring `describe_tool` (ADR-0045 D6).
