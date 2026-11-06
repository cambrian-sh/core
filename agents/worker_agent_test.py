"""Tests for the profile-driven worker runtime (agents/worker_agent.py).

Named *_test.py, not test_*agent.py: the kernel's agent seeder registers every file
ending in "agent.py" as an agent.

Each starter profile is started against a fake kernel that serves MORE tools and skills
than the profile allows, and drives the real SDK loop (run_think) over native tool
calling, so the assertions are on what would actually reach the model: the tool
definitions and the skill menu.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import worker_agent as wa  # noqa: E402
from cambrian_agent_sdk.profiles import ProfileScopedSubstrate, load_profile  # noqa: E402
from cambrian_agent_sdk.react import sanitize_tool_name  # noqa: E402
from cambrian_agent_sdk.types import AgentTask  # noqa: E402

_HERE = Path(__file__).resolve().parent
_SKILLS = _HERE.parent / "skills"

# Every tool the curated MCP set (configs/mcp.json.example) exposes, plus a native one.
_ALL_TOOLS = [
    "mcp:fetch/fetch",
    "mcp:web-search/search", "mcp:web-search/fetch_content",
    "mcp:filesystem/read_text_file", "mcp:filesystem/read_file", "mcp:filesystem/read_media_file",
    "mcp:filesystem/read_multiple_files", "mcp:filesystem/list_directory",
    "mcp:filesystem/list_directory_with_sizes", "mcp:filesystem/directory_tree",
    "mcp:filesystem/search_files", "mcp:filesystem/get_file_info",
    "mcp:filesystem/list_allowed_directories", "mcp:filesystem/write_file",
    "mcp:filesystem/edit_file", "mcp:filesystem/move_file",
    "mcp:code-sandbox/execute_code", "mcp:code-sandbox/get_supported_languages",
    "mcp:shell/shell_execute",
    "mcp:git/git_status", "mcp:git/git_commit",
    "mcp:sqlite/list_tables", "mcp:sqlite/execute_sql",
    "mcp:time/get_current_time",
    "mcp:pdf-reader/read",
    "mcp:markitdown/convert_to_markdown",
    "mcp:sequential-thinking/sequentialthinking",
    "web_search",
]

_ALL_SKILLS = ["file-report", "summarise", "web-research", "exploratory-analysis",
               "chart-report", "ops-runbook"]

_SCHEMA = '{"type":"object","properties":{"path":{"type":"string"}}}'


class _Kernel:
    """ListTools / ListSkills / ExecuteTool / native generation shaped like the kernel's,
    serving everything (as tools_unrestricted would) so only the profile can narrow it."""

    def __init__(self, script):
        self.script = list(script)
        self.tool_calls = []
        self.skill_calls = []
        self.tools_sent = []
        self.systems = []
        self.executed = []

    def list_tools(self, query="", k=0, names=None, full=False, session_token_id=""):
        self.tool_calls.append({"query": query, "names": names, "full": full})
        return [{"name": n, "description": f"tool {n}", "schema_json": _SCHEMA}
                for n in _ALL_TOOLS if not names or n in names]

    def list_skills(self, query="", k=0, names=None, full=False, session_token_id=""):
        self.skill_calls.append({"query": query, "names": names, "full": full})
        out = []
        for n in names or _ALL_SKILLS:
            if n not in _ALL_SKILLS:
                continue
            d = {"name": n, "description": f"{n} procedure."}
            if full:
                d.update(instructions=f"Follow the {n} steps.",
                         tool_grants=["mcp:shell/shell_execute", "mcp:filesystem/read_file"])
            out.append(d)
        return out

    def execute_tool(self, name, args_json="", session_token_id="", timeout_ms=0, task_id=""):
        self.executed.append(name)
        return {"result_json": json.dumps({"ok": True})}

    def generate_with_tools(self, session_token_id=None, messages=None, tools=None,
                            system=None, **kw):
        self.tools_sent.append([t["name"] for t in tools])
        self.systems.append(json.dumps(system or []) + json.dumps(messages or []))
        step = self.script.pop(0) if self.script else ("done", [])
        if callable(step):
            step = step(tools)
        text, calls = step
        return (text, calls, "tool_use" if calls else "end_turn")


def _call(name, args):
    return ("", [{"id": f"c-{name}", "name": name, "arguments": json.dumps(args)}])


def _san(original):
    """The provider-safe name the loop gives a kernel tool (react.sanitize_tool_name)."""
    return sanitize_tool_name(original, {})


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch):
    for k in (wa.PROFILE_ENV, wa.PROFILES_DIR_ENV, wa.SKILLS_DIR_ENV, "CAMBRIAN_AGENT_ID"):
        monkeypatch.delenv(k, raising=False)


@pytest.mark.parametrize("name", ["assistant", "researcher", "data-analyst"])
def test_starter_profile_advertises_only_allowlisted_tools_and_its_skills(name):
    agent = wa.build_agent(["worker_agent.py", "--profile", name])
    prof = agent.profile
    assert agent.agent_id == name
    assert agent.role == prof.role
    assert agent.description == prof.description

    kernel = _Kernel([_call("use_skill", {"skill": prof.skills[0]}), ("done", [])])
    agent.substrate = kernel
    assert isinstance(agent.substrate, ProfileScopedSubstrate)

    res = agent.think(AgentTask(text="help me with this", session_token_id="sess-1"))
    assert res.text == "done"

    # The seeded menu was the FULL granted set (query ""), narrowed by the allowlist.
    assert kernel.tool_calls[0]["query"] == ""
    allowed = {n for n in _ALL_TOOLS if prof.allowlist.allows(n)}
    assert allowed, "a starter profile must reach at least one curated tool"
    want = {_san(n) for n in allowed}
    for sent in kernel.tools_sent:
        offered = {t for t in sent if t.startswith("mcp_") or t == "web_search"}
        assert offered == want, (name, offered ^ want)

    # The skill menu is the agent's effective set, Tier-1: the kernel's full listing
    # (decisions/skill-access-grants.md: profile skills + grants, enforced and marked
    # kernel-side). This fake kernel marks nothing, like an older one, so the profile
    # proxy bounds the menu to the profile's skills; no other skill is listed.
    menu_call = kernel.skill_calls[0]
    assert menu_call["names"] is None and menu_call["full"] is False
    prompt = kernel.systems[0]
    for s in prof.skills:
        assert s in prompt
    for s in set(_ALL_SKILLS) - set(prof.skills):
        assert f"{s} procedure" not in prompt, s
    # The use_skill load fetched Tier-2 for the profile's skill only.
    assert {"query": "", "names": [prof.skills[0]], "full": True} in kernel.skill_calls


def test_out_of_profile_tool_call_is_refused_locally():
    agent = wa.build_agent(["worker_agent.py", "--profile", "researcher"])
    agent.substrate = kernel = _Kernel([])
    resp = agent.substrate.execute_tool("mcp:code-sandbox/execute_code", args_json="{}")
    assert resp["denied"]
    assert kernel.executed == []
    agent.substrate.execute_tool("mcp:fetch/fetch", args_json="{}")
    assert kernel.executed == ["mcp:fetch/fetch"]


def test_limits_come_from_the_profile(monkeypatch):
    agent = wa.build_agent(["worker_agent.py", "--profile", "data-analyst"])
    seen = {}

    def fake_run_think(a, task, **kw):
        seen.update(kw)
        return "ok"

    import cambrian_agent_sdk.react as react
    monkeypatch.setattr(react, "run_think", fake_run_think)
    agent.think(AgentTask(text="x"))
    assert seen["max_tool_rounds"] == agent.profile.max_tool_rounds == 20
    assert seen["role"] == agent.profile.role


def test_profile_selection_order(monkeypatch):
    assert wa.resolve_profile_name(["w"]) == wa.DEFAULT_PROFILE
    monkeypatch.setenv(wa.PROFILE_ENV, "researcher")
    assert wa.resolve_profile_name(["w"]) == "researcher"
    assert wa.resolve_profile_name(["w", "--profile", "data-analyst"]) == "data-analyst"


def test_agent_id_is_the_launched_id(monkeypatch):
    prof = load_profile("researcher", profiles_dir=_HERE / "profiles", skills_dir=_SKILLS)
    assert wa.resolve_agent_id(prof, ["w"]) == "researcher"
    monkeypatch.setenv("CAMBRIAN_AGENT_ID", "worker_agent")
    assert wa.resolve_agent_id(prof, ["w"]) == "worker_agent"
    assert wa.resolve_agent_id(prof, ["w", "--agent-id", "r2"]) == "r2"


def test_invalid_profile_fails_boot(tmp_path, monkeypatch, capsys):
    (tmp_path / "broken.yaml").write_text("name: broken\nbogus: 1\n", encoding="utf-8")
    monkeypatch.setenv(wa.PROFILES_DIR_ENV, str(tmp_path))
    assert wa.main(["w", "--profile", "broken"]) == 2
    assert "unknown field" in capsys.readouterr().err
    assert wa.main(["w", "--profile", "missing"]) == 2


def test_runtime_file_carries_no_registration_literals():
    """Part B: the kernel registers one agent per profile and never this file, so the
    profile is the only source of the description and manifest. A literal here would be
    a second copy to drift (and a regex reader would take it for an agent)."""
    src = (_HERE / "worker_agent.py").read_text(encoding="utf-8")
    assert "AGENT_DESCRIPTION =" not in src
    assert "AGENT_MANIFEST =" not in src


def test_profile_manifest_keeps_allowlist_out_of_legacy_tools_key():
    prof = load_profile(wa.DEFAULT_PROFILE, profiles_dir=_HERE / "profiles", skills_dir=_SKILLS)
    manifest = prof.manifest()
    assert "tools" not in manifest  # the kernel reads manifest tools as capability tags
    assert manifest["tool_allowlist"] == list(prof.tools)
