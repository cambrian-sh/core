"""Golden renders of every retrieval_agent request (ADR-0139 D2).

Each op's request (system blocks with their cache marks, the user message, the
response schema name, the prompt version) is rendered from fixed inputs and
compared with ``golden/<case>.json``. A prompt edit, a reordered region or a lost
data tag shows up here as a diff. A deliberate change: bump the prompt version
(cambrian-core/prompts/README.md), then regenerate with
``RETRIEVAL_GOLDEN_UPDATE=1 python -m pytest -q test_golden.py`` and review the diff.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

import planner

GOLDEN = Path(__file__).with_name("golden")
CHUNKS = ["Alice reports to Bob Chen.", "Bob Chen approved the Titan migration.",
          "Alice reports to Bob Chen."]


def _cases():
    q = "What did the person Alice reports to approve?"
    return {
        "plan_step_first": planner.plan_step_request(q),
        "plan_step_hop": planner.plan_step_request(q, "Bob Chen", ["Alice", "Bob Chen"], 2),
        "decide_continue": _capture(lambda llm: planner.decide_continue(
            {"query": q, "chunks": CHUNKS, "history": ["Alice"]}, llm)),
        "synthesize": planner.synthesize_request(q, CHUNKS, cited=False),
        "synthesize_cited": planner.synthesize_request(q, CHUNKS, cited=True),
        "decompose": planner.decompose_request(q, routes=False),
        "decompose_routed": planner.decompose_request(q, routes=True),
        "answer_subq": _capture(lambda llm: planner.answer_subquestion("Who approved it?", CHUNKS, llm)),
    }


def _capture(fn):
    seen = []
    fn(lambda req: seen.append(req) or planner.Reply("", schema_checked=True))
    return seen[0]


def _as_json(req: planner.LLMRequest) -> dict:
    return {
        "op": req.op,
        "system": req.system,
        "messages": req.messages,
        "response_schema": None if req.response_schema is None else req.response_schema["name"],
        "prompt_version": req.prompt_version,
    }


@pytest.mark.parametrize("name", sorted(_cases()))
def test_golden_render(name):
    got = _as_json(_cases()[name])
    path = GOLDEN / f"{name}.json"
    if os.environ.get("RETRIEVAL_GOLDEN_UPDATE") == "1":
        GOLDEN.mkdir(exist_ok=True)
        path.write_text(json.dumps(got, indent=2, ensure_ascii=False) + "\n", encoding="utf-8", newline="\n")
    want = json.loads(path.read_text(encoding="utf-8"))
    assert got == want, f"{name}: render changed; see the module docstring"


def test_documents_precede_the_question_in_every_data_bearing_request():
    for name, req in _cases().items():
        u = req.user
        if "<documents>" in u:
            assert u.index("</documents>") < u.rindex("<question>" if "<question>" in u else "<sub_question>"), name
        assert all(m["role"] == "user" for m in req.messages), name
        assert req.system[-1]["cache"] is True, name
