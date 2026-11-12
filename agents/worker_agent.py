"""Worker agent: one runtime for every declarative profile (agent stack v2).

Owner directive decisions/agent-stack-v2-mcp-tools-and-profiles.md D3: specialists are
declarative profiles over ONE runtime. A profile (``agents/profiles/<name>.yaml``, format
in ``agents/profiles/README.md``) carries the agent id, routing text, capabilities,
skills, tool allowlist, model preference, trust tier, limits and a short role. This file
holds no persona and no method: the role comes from the profile, and the method comes
from the profile's skills (``skills/<name>/SKILL.md``), loaded on demand.

Started as::

    python worker_agent.py --profile data-analyst [--socket ... --substrate-addr ...]

The profile is chosen by ``--profile``, then ``$CAMBRIAN_AGENT_PROFILE`` (manual runs
only: the kernel's deny-by-default agent environment does not pass ``CAMBRIAN_*``), then
:data:`DEFAULT_PROFILE`. The agent id is the id the kernel launched the process as
(``--agent-id``, then ``$CAMBRIAN_AGENT_ID``), else the profile's ``name``.

What it does with the profile:

- **Role and limits.** ``role`` is the prompt's <Role>; ``max_tool_rounds`` and
  ``max_tokens`` bound the ReAct loop.
- **Skills.** The skill menu is the agent's effective skill set: the profile's skills
  plus the skills an operator granted this agent id (decisions/skill-access-grants.md),
  as Tier-1 index lines served and ENFORCED by the kernel. A body loads on demand
  through ``use_skill`` and is pinned for the rest of the run (ADR-0139 D3 Phase 0).
  ``find_skills`` may discover skills outside the set; ``use_skill`` on one asks an
  operator, and an approval is a durable grant (the body loads in the same run).
- **Tools.** The tool menu is the kernel's granted tools filtered to the profile's
  allowlist globs, and a call outside the allowlist is refused locally. The client
  filter keeps the menu small; the ENFORCING control is the kernel's: it grants a
  profile agent exactly its allowlist's expansion over the tool registry and holds the
  same globs as a hard ceiling, also under ``tools_unrestricted``
  (app/profile_agents.go, domain/tool_ceiling.go).

**Composed mode (ADR-0145 D12).** ``--composed`` runs the one runtime the kernel
composes plan-step workers on (``execution.workers.composed``). It has no profile: for
each step the kernel sends the composition (``_composition``: the professions' skills,
the tools inside their allowlists AND the person's scope, the brief, the input ledger
references) and binds it to the step's lease, where it is enforced. A step's declared
effects no longer narrow its tools (ADR-0145 D14).

**More work than the step (ADR-0145 D14).** A worker that finds work beyond its step
hands it to the planner with the local tool ``delegate_to_planner``, exactly as the chat
agent delegates: a kernel Execute carrying the step's lease, so the kernel plans it,
applies the person's access and approvals, bounds the nesting
(``execution.plan.max_worker_delegation_depth``) and records it. There are no subagents
(``spawn_subagent`` was removed) and an agent never addresses another agent
(decisions/all-agent-communication-passes-through-the-kernel.md). The role and rules are developer-written (``prompts/composed_worker``); the
composition is data. Without a composition the runtime refuses the task.

**How the kernel sees this file.** The kernel registers one agent per valid profile
(internal/storage/profile_discovery.go): id = the profile name, this file as the exec
path, launched with ``--profile <name> --agent-id <id>``, described and routed by the
profile. This file itself is never registered, so it carries no AGENT_DESCRIPTION or
AGENT_MANIFEST: the profile is the only source of both.
"""

from __future__ import annotations

import logging
import os
import sys
from pathlib import Path
from typing import List, Optional, Sequence

from cambrian_agent_sdk import AgentResult, CognitiveAgent, tool
from cambrian_agent_sdk._logging import configure_logging
from cambrian_agent_sdk.composition import ComposedScopedSubstrate, composition_from_task
from cambrian_agent_sdk.prompt_library import PromptLibrary, sections
from cambrian_agent_sdk.profiles import (
    InvalidProfile,
    Profile,
    ProfileScopedSubstrate,
    load_profile,
    manifest_json,
)

_log = logging.getLogger("cambrian.worker_agent")

#: The profile a bare launch (no ``--profile``, no env) runs.
DEFAULT_PROFILE = "assistant"

PROFILE_ENV = "CAMBRIAN_AGENT_PROFILE"
PROFILES_DIR_ENV = "CAMBRIAN_PROFILES_DIR"
SKILLS_DIR_ENV = "CAMBRIAN_SKILLS_DIR"


def profiles_dir() -> Path:
    env = os.environ.get(PROFILES_DIR_ENV, "").strip()
    return Path(env) if env else Path(__file__).resolve().parent / "profiles"


def skills_dir() -> Optional[Path]:
    """``$CAMBRIAN_SKILLS_DIR``, else the nearest ancestor ``skills/`` holding a SKILL.md
    (the repo and the image both put skills/ beside agents/, as the kernel reads it
    relative to its working directory). None when there is none: the kernel is the
    authority on skills, so the runtime then skips the local existence check."""
    env = os.environ.get(SKILLS_DIR_ENV, "").strip()
    if env:
        return Path(env)
    for parent in Path(__file__).resolve().parents:
        cand = parent / "skills"
        if cand.is_dir() and any(cand.glob("*/SKILL.md")):
            return cand
    return None


def _flag(argv: Sequence[str], name: str) -> str:
    if name in argv:
        i = list(argv).index(name)
        if i + 1 < len(argv):
            return argv[i + 1]
    return ""


def resolve_profile_name(argv: Optional[Sequence[str]] = None) -> str:
    argv = sys.argv if argv is None else argv
    return (_flag(argv, "--profile") or os.environ.get(PROFILE_ENV, "").strip()
            or DEFAULT_PROFILE)


def resolve_agent_id(profile: Profile, argv: Optional[Sequence[str]] = None) -> str:
    argv = sys.argv if argv is None else argv
    return (_flag(argv, "--agent-id") or os.environ.get("CAMBRIAN_AGENT_ID", "").strip()
            or profile.name)


class PlannerDelegation:
    """ADR-0145 D14: the worker's way to hand extra work to the planner.

    The same kernel path the chat agent's ``delegate_to_planner`` takes (Execute with
    the caller's lease), so all delegation passes through the kernel. A refusal or a
    failure comes back as such, never as a finished result."""

    _step_lease = ""

    def _bind_step(self, task) -> None:
        self._step_lease = getattr(task, "session_token_id", "") or ""

    @tool
    def delegate_to_planner(self, task: str):
        """Hand work that is outside your step to the planner and return its result: a
        separate lookup, record or action your step needs but was not asked to do. Pass
        the whole sub-task with every limit the person set. Do not use it for your own
        step's work, and never to repeat a write that already happened."""
        task = (task or "").strip()
        if not task:
            return {"error": "delegate_to_planner needs the sub-task to hand over."}
        substrate = getattr(self, "substrate", None)
        if substrate is None or not hasattr(substrate, "execute"):
            return {"error": "delegation is not available here (no kernel connection)."}
        try:
            result = substrate.execute(task, session_token_id=self._step_lease)
        except Exception as exc:  # noqa: BLE001 — a refusal is data for the loop, never a success
            details = getattr(exc, "details", None)
            detail = details() if callable(details) else str(exc)
            _log.warning("worker_delegation_refused", extra={"detail": str(detail)[:300]})
            # A dict with "error": the loop records it as a failed call (its status,
            # the recurrence gate), never as a finished result.
            return {"error": f"denied: the planner did not take the work: {detail}"}
        text = str(getattr(result, "text", result) or "")
        failed = (getattr(result, "partial", False) or getattr(result, "timed_out", False)
                  or getattr(result, "error", "") or getattr(result, "failed_step", -1) not in (-1, None))
        if failed:
            why = getattr(result, "error", "") or "it stopped before every step completed"
            return {"error": f"the delegated work did not finish ({why}). What it reported: {text}"}
        return text

class WorkerAgent(PlannerDelegation, CognitiveAgent):
    """A CognitiveAgent configured entirely by a :class:`Profile`."""

    #: Seed the FULL granted menu (query="") and let the allowlist bound it, instead of
    #: the ADR-0044 task-relevant top-k: a top-k over the whole registry, filtered
    #: afterwards, could leave a profile with none of its own tools on the menu.
    seed_tools_full = True

    #: ADR-0139 D14 / D13: a worker may write principal memory notes and propose
    #: skills. Each verb is offered only when the kernel serves it.
    allow_memory_note_writes = True
    allow_skill_proposals = True

    def __init__(self, profile: Profile, agent_id: Optional[str] = None) -> None:
        super().__init__(agent_id or profile.name, version=profile.version,
                         description=profile.description)
        self.profile = profile
        self.role = profile.role
        if profile.max_tokens is not None:
            self.max_tokens = profile.max_tokens
        self._substrate = None

    # The substrate is wrapped on assignment, whoever assigns it (``_bind_clients`` at
    # serve time, or a test), so there is no unscoped window.
    @property
    def substrate(self):
        return self._substrate

    @substrate.setter
    def substrate(self, value) -> None:
        if value is not None and not isinstance(value, ProfileScopedSubstrate):
            value = ProfileScopedSubstrate(value, self.profile)
        self._substrate = value

    def think(self, task, *, max_memory_queries=None, max_tool_rounds=None):
        self._bind_step(task)
        if max_tool_rounds is None:
            max_tool_rounds = self.profile.max_tool_rounds
        return super().think(task, max_memory_queries=max_memory_queries,
                             max_tool_rounds=max_tool_rounds)

    def manifest_json(self) -> str:
        return manifest_json(self.profile)


#: The agent id a bare ``--composed`` launch runs as (config.DefaultComposedRuntimeAgentID).
COMPOSED_AGENT_ID = "composed_worker"


def prompts_dir() -> Path:
    """``$CAMBRIAN_PROMPTS_DIR``, else the nearest ancestor ``prompts/`` holding the
    composed worker's prompt (the repo and the image both put prompts/ beside agents/)."""
    env = os.environ.get("CAMBRIAN_PROMPTS_DIR", "").strip()
    if env:
        return Path(env)
    for parent in Path(__file__).resolve().parents:
        if (parent / "prompts" / "composed_worker" / "v1.md").is_file():
            return parent / "prompts"
    raise RuntimeError("worker_agent --composed: cannot find the kernel prompt library; set CAMBRIAN_PROMPTS_DIR")


#: The composed runtime's per-step tool-call budget, set by the kernel from
#: execution.workers.max_tool_rounds (internal/kernel/trace_env.go workersEnv).
COMPOSED_MAX_TOOL_ROUNDS_ENV = "CAMBRIAN_COMPOSED_MAX_TOOL_ROUNDS"


def composed_max_tool_rounds() -> Optional[int]:
    """The configured budget, or None (the SDK default) when unset or not a positive int."""
    raw = os.environ.get(COMPOSED_MAX_TOOL_ROUNDS_ENV, "").strip()
    if not raw:
        return None
    try:
        n = int(raw)
    except ValueError:
        _log.warning("composed_max_tool_rounds_invalid", extra={"value": raw})
        return None
    return n if n > 0 else None


class ComposedWorkerAgent(PlannerDelegation, CognitiveAgent):
    """The ADR-0145 D12 runtime: one ReAct loop, composed per step by the kernel."""

    #: The composition is the menu: the kernel serves it whole for the step's lease.
    seed_tools_full = True
    allow_memory_note_writes = True
    allow_skill_proposals = True
    #: A plan-step runtime: a step that needs a tool its composition lacks ends as
    #: capability_missing and is re-planned with a worker composed for the need (a
    #: sub-goal would run under this same composition).
    #: T-20261112-yield-sub-goal-on-a-composed-lease-runs-under.
    allow_capability_missing = True

    def __init__(self, agent_id: str = COMPOSED_AGENT_ID, prompts: Optional[Path] = None) -> None:
        super().__init__(agent_id, description="Composed worker runtime (ADR-0145 D12).")
        text = sections(PromptLibrary.load(prompts or prompts_dir()).get("composed_worker"))
        # Developer-written and fixed for every step: the system block stays cacheable.
        self.role = text["role"].render()
        self.constraints = [ln for ln in text["rules"].render().splitlines() if ln.strip()]
        self._substrate = None

    @property
    def substrate(self):
        return self._substrate

    @substrate.setter
    def substrate(self, value) -> None:
        if value is not None and not isinstance(value, ComposedScopedSubstrate):
            value = ComposedScopedSubstrate(value)
        self._substrate = value

    def think(self, task, *, max_memory_queries=None, max_tool_rounds=None):
        # execution.workers.max_tool_rounds, set by the kernel in the agent's
        # environment; unset ⇒ the SDK default (T-20261109-per-step-deadline-step-timeout-floor-ms-180-s).
        if max_tool_rounds is None:
            max_tool_rounds = composed_max_tool_rounds()
        return super().think(task, max_memory_queries=max_memory_queries, max_tool_rounds=max_tool_rounds)

    def run(self, task):
        comp = composition_from_task(task)
        if comp is None:
            # The kernel never dispatches to the runtime without one; a task without
            # it gets no tools and no skills here or kernel-side, so refuse plainly.
            _log.error("composed_worker_no_composition", extra={"agent_id": self.agent_id})
            # A refusal, never a finished result (ADR-0145 D14).
            return AgentResult.from_text(
                "This worker runs only steps the kernel composed; the task carried no composition.",
                type="error")
        self._bind_step(task)
        if self._substrate is not None:
            self._substrate.bind(comp, getattr(task, "session_token_id", "") or "")
        _log.info("composed_worker_step", extra={
            "agent_id": self.agent_id, "professions": list(comp.professions),
            "skills": len(comp.skills), "tools": len(comp.tools), "step": comp.step})
        try:
            return self.think(task)
        finally:
            if self._substrate is not None:
                self._substrate.bind(None, "")


def is_composed(argv: Optional[Sequence[str]] = None) -> bool:
    argv = sys.argv if argv is None else argv
    return "--composed" in argv


def build_agent(argv: Optional[Sequence[str]] = None) -> CognitiveAgent:
    """Resolve and validate the profile, then construct the agent. An invalid profile
    raises :class:`InvalidProfile`; the caller (``__main__``) exits non-zero on it, so a
    bad profile fails at boot, loudly, instead of serving with a guessed configuration.
    ``--composed`` builds the ADR-0145 D12 runtime instead (no profile)."""
    if is_composed(argv):
        a = sys.argv if argv is None else argv
        return ComposedWorkerAgent(_flag(a, "--agent-id") or os.environ.get("CAMBRIAN_AGENT_ID", "").strip()
                                   or COMPOSED_AGENT_ID)
    name = resolve_profile_name(argv)
    sdir = skills_dir()
    if sdir is None:
        _log.warning("worker_skills_dir_not_found",
                     extra={"profile": name, "detail": "skipping the local skill check; "
                            "the kernel still serves only skills it has"})
    profile = load_profile(name, profiles_dir=profiles_dir(), skills_dir=sdir)
    for w in profile.warnings:
        _log.warning("worker_profile_warning", extra={"profile": profile.name, "detail": w})
    return WorkerAgent(profile, agent_id=resolve_agent_id(profile, argv))


def main(argv: Optional[List[str]] = None) -> int:
    try:
        agent = build_agent(argv)
    except InvalidProfile as exc:
        configure_logging(agent_id="worker_agent")
        _log.error("worker_profile_invalid", extra={"error": str(exc)})
        print(f"worker_agent: {exc}", file=sys.stderr)
        return 2
    configure_logging(agent_id=agent.agent_id)
    _log.info("worker_agent_start", extra={
        "profile": agent.profile.name if isinstance(agent, WorkerAgent) else "(composed)",
        "agent_id": agent.agent_id})
    agent.serve()
    return 0


if __name__ == "__main__":
    sys.exit(main())
