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

from cambrian_agent_sdk import CognitiveAgent
from cambrian_agent_sdk._logging import configure_logging
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


class WorkerAgent(CognitiveAgent):
    """A CognitiveAgent configured entirely by a :class:`Profile`."""

    #: Seed the FULL granted menu (query="") and let the allowlist bound it, instead of
    #: the ADR-0044 task-relevant top-k: a top-k over the whole registry, filtered
    #: afterwards, could leave a profile with none of its own tools on the menu.
    seed_tools_full = True

    #: ADR-0139 D12 / D14 / D13: a worker may spawn subagents, write principal memory
    #: notes and propose skills (owner-recorded default, ticket
    #: T-20261105-adr-0139-d12-in-agent-subagents-via-spawn). Each verb is offered only
    #: when the kernel serves it; the kernel bounds what a child may do.
    allow_subagents = True
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
        if max_tool_rounds is None:
            max_tool_rounds = self.profile.max_tool_rounds
        return super().think(task, max_memory_queries=max_memory_queries,
                             max_tool_rounds=max_tool_rounds)

    def manifest_json(self) -> str:
        return manifest_json(self.profile)


def build_agent(argv: Optional[Sequence[str]] = None) -> WorkerAgent:
    """Resolve and validate the profile, then construct the agent. An invalid profile
    raises :class:`InvalidProfile`; the caller (``__main__``) exits non-zero on it, so a
    bad profile fails at boot, loudly, instead of serving with a guessed configuration."""
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
    _log.info("worker_agent_start", extra={"profile": agent.profile.name,
                                           "agent_id": agent.agent_id})
    agent.serve()
    return 0


if __name__ == "__main__":
    sys.exit(main())
