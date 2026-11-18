"""ENG-126: the single-agent lane's chat agent sends a cacheable prefix that holds
across the turns of a conversation.

The provider caches a prompt prefix only while its bytes are unchanged. On Anthropic
the prefix is the tool definitions, then the system blocks (the breakpoint is on the
last cache-marked block); OpenAI-compatible endpoints and the claudebox proxy cache an
unchanged system prompt. Each chat turn is a fresh run, so everything in that prefix
must be fixed for the conversation: the transcript, the latest message, today's date
and recalled memory belong in the user turn, and the tool and skill menus must not be
a per-message top-k.

Named *_test.py, not test_*agent.py: the kernel's agent seeder registers every file
ending in "agent.py" as an agent.
"""
from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import chat_agent as ca  # noqa: E402
from cambrian_agent_sdk.types import AgentTask  # noqa: E402

from chat_agent_lane_test import LANE  # noqa: E402

_SCHEMA = json.dumps({"type": "object", "properties": {"customer": {"type": "string"}}})
_TOOLS = ["mcp:crm/contacts", "mcp:erp/invoices", "mcp:filesystem/read_file"]
_SKILLS = ["chart-report", "file-report", "summarise", "web-research"]


class _LaneKernel:
    """The kernel surface the chat agent reaches, shaped like the real one: ListTools
    with no query is the full menu sorted by name (ENG-126), ListSkills with a query
    is relevance-ranked top-k (the skill named in the query first), without one the
    full menu; native generation records what the provider would receive."""

    def __init__(self):
        self.calls = []  # one {system, tools, cache_ttl, messages} per model call
        self.skill_queries = []

    def list_tools(self, query="", k=0, names=None, full=False, session_token_id=""):
        names_ = [n for n in _TOOLS if not names or n in names]
        if query:  # a top-k push would rank by the query; the lane must not ask for one
            names_ = names_[:1]
        return [{"name": n, "description": f"tool {n}", "schema_json": _SCHEMA} for n in names_]

    def list_skills(self, query="", k=0, names=None, full=False, session_token_id=""):
        self.skill_queries.append(query)
        menu = [{"name": n, "description": f"The {n} procedure."} for n in _SKILLS]
        if names:
            return [s for s in menu if s["name"] in names]
        if query:
            ranked = sorted(menu, key=lambda s: s["name"] not in query)
            return ranked[: (k or 3)]
        return menu

    def generate_with_tools(self, session_token_id=None, messages=None, tools=None,
                            system=None, cache_ttl=None, **kw):
        self.calls.append({"system": json.dumps(system or [], sort_keys=True),
                           "tools": json.dumps(tools or [], sort_keys=True),
                           "cache_ttl": cache_ttl, "messages": json.dumps(messages or [])})
        return ("", [{"id": "c1", "name": "final_answer",
                      "arguments": json.dumps({"answer": "Done.", "type": "text"})}], "tool_use")


def _turn(kernel, text, transcript, today):
    a = ca.ChatAgent(agent_id="chat_test", version="1.0.0", description="t")
    a.substrate = kernel
    a.memory = None
    task = AgentTask(text=text, type="text", context=dict(LANE), session_token_id="lease-1",
                     metadata={"language": "en", "policy": "Be kind.", "transcript": transcript,
                               "current_date": today, "recall_lookup": "false"})
    return a.run(task)


def test_lane_chat_turns_send_a_byte_identical_cacheable_prefix():
    kernel = _LaneKernel()
    _turn(kernel, "summarise my invoices", "", "Today is 2026-11-18.")
    _turn(kernel, "make a chart-report of what ACME owes",
          "customer: summarise my invoices\nagent: Done.", "Today is 2026-11-19.")
    first, second = kernel.calls[0], kernel.calls[-1]

    # The provider-visible prefix: tool definitions, then the cache-marked system tier.
    assert first["tools"] == second["tools"], "tool definitions changed between turns"
    assert first["system"] == second["system"], "the cached system tier changed between turns"
    system = json.loads(first["system"])
    assert len(system) == 1 and system[0]["cache"] is True
    # A chat is human-paced: the long cache window.
    assert first["cache_ttl"] == second["cache_ttl"] == "long"

    # The full menus, not a per-message top-k (the kernel serves them sorted).
    assert all(q == "" for q in kernel.skill_queries), kernel.skill_queries
    text = system[0]["text"]
    for n in _SKILLS:
        assert n in text, f"skill {n} missing from the full menu"
    sent = [t["name"] for t in json.loads(first["tools"])]
    assert sum(1 for n in sent if n.startswith("mcp_")) == len(_TOOLS), sent

    # What does change per turn is in the user turn, outside the cached prefix.
    assert "2026-11-18" not in text and "2026-11-19" not in text
    assert "summarise my invoices" not in text and "chart-report of what ACME owes" not in text
    assert "2026-11-19" in second["messages"] and "chart-report of what ACME owes" in second["messages"]
