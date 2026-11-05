"""The decompose op: the legacy plan and the ADR-0135 routed plan.

Fake LLMs only — no model, no kernel. The op-level routing (routes=true vs the
legacy shape) is covered in test_agent.py.
"""
from __future__ import annotations

import json

import planner
from planner import Reply


def const(text: str, checked: bool = False):
    return lambda req: Reply(text, schema_checked=checked)


ROUTED = planner.LIBRARY.get("retrieval_decompose_routed").body
LEGACY = planner.LIBRARY.get("retrieval_decompose").body


def test_routed_prompt_carries_routes_examples_and_the_facets_rule():
    for needle in ('"direct"', '"facets"', '"chain"', "{1}",
                   "Which vendor did we choose for the office move?",
                   "How many incidents did the payments team report this quarter?",
                   "Which office does the engineer who wrote the deployment runbook work in?",
                   "each one stands alone", "the route is chain"):
        assert needle in ROUTED, needle
    assert '"route"' not in LEGACY


def test_no_prompt_carries_a_benchmark_question():
    # T-20261102-retrieval-agent-planner-prompts-use-benchmark: few-shot examples must
    # never be benchmark test questions.
    every = " ".join(planner.LIBRARY.get(i).body for i in planner.LIBRARY.ids() if i.startswith("retrieval_"))
    for leaked in ("Where do I take yoga classes?", "What is the name of my cat?",
                   "How many graduation ceremonies have I attended",
                   "Kiwil", "Commission of Truth and Friendship", "Stadio Ciro Vigorito",
                   "Air Defense Artillery", "Genesis's advantages", "the Rams' league"):
        assert leaked not in every, leaked


def test_routed_request_shape():
    seen = []
    planner.decompose_routed("Which vendor?", lambda req: seen.append(req) or Reply('{"route":"direct","sub_questions":[]}', True))
    req = seen[0]
    assert req.system == [{"text": ROUTED, "cache": True}]
    assert req.response_schema["name"] == "retrieval_decompose_routed"
    assert "<question>\nWhich vendor?\n</question>" in req.user


def test_well_formed_object_per_route():
    direct = planner.decompose_routed("Which vendor did we choose?", const('{"route": "direct", "sub_questions": []}', True))
    assert direct == {"route": "direct", "sub_questions": ["Which vendor did we choose?"], "refs": [""]}

    facets = planner.decompose_routed("Which first?", const(json.dumps({
        "route": "facets",
        "sub_questions": [{"q": "When did the app ship?", "ref": "app date"},
                          {"q": "When did the dashboard ship?", "ref": "dashboard date"}]}), True))
    assert facets["route"] == "facets"
    assert facets["sub_questions"] == ["When did the app ship?", "When did the dashboard ship?"]
    assert facets["refs"] == ["app date", "dashboard date"]

    chain = planner.decompose_routed("Which office?", const(
        "```json\n" + json.dumps({"route": "Chain", "sub_questions": [
            {"q": "Who wrote the runbook?", "ref": "the author"},
            {"q": "Which office does {1} work in?", "ref": "the office of {1}"}]}) + "\n```"))
    assert chain["route"] == "chain"
    assert chain["sub_questions"][1] == "Which office does {1} work in?"


def test_defensive_parsing():
    legacy = planner.decompose_routed("q?", const('[{"q": "a?", "ref": "A"}, {"q": "b of {1}?", "ref": "B"}]'))
    assert legacy["route"] == "" and legacy["sub_questions"] == ["a?", "b of {1}?"]
    assert planner.decompose_routed("q?", const('[{"q": "a?", "ref": "A"}]'))["sub_questions"] == ["a?"]
    # A checked reply is never scraped for a bare list (the schema says object).
    assert planner.decompose_routed("q?", const('[{"q": "a?"}]', True))["sub_questions"] == ["q?"]
    assert planner.decompose_routed("Where?", const("I cannot help with that.")) == {
        "route": "", "sub_questions": ["Where?"], "refs": [""]}
    missing = planner.decompose_routed("q?", const('{"sub_questions": [{"q": "a?"}]}'))
    assert missing["route"] == "" and missing["sub_questions"] == ["a?"]
    unknown = planner.decompose_routed("q?", const('{"route": "Parallel", "sub_questions": ["a?", "b?"]}'))
    assert unknown["route"] == "parallel"
    assert unknown["sub_questions"] == ["a?", "b?"] and unknown["refs"] == ["", ""]
    assert planner.decompose_routed("q?", const('{"route": 7, "sub_questions": "a?"}')) == {
        "route": "", "sub_questions": ["q?"], "refs": [""]}
    junk = planner.decompose_routed("q?", const('{"route": "facets", "sub_questions": [null, {"q": ""}, ["x"], {"q": "a?"}]}'))
    assert junk["sub_questions"] == ["a?"]
    many = planner.decompose_routed("q?", const(json.dumps(
        {"route": "facets", "sub_questions": [{"q": f"s{i}?"} for i in range(9)]})))
    assert len(many["sub_questions"]) == 4 and len(many["refs"]) == 4
    assert planner.decompose_routed("  ", const("x"))["sub_questions"] == []


def test_legacy_decompose_object_and_old_list():
    obj = planner.decompose("q?", const('{"sub_questions": [{"q": "a?", "ref": "A"}, {"q": "b {1}?", "ref": "B"}]}', True))
    assert obj == (["a?", "b {1}?"], ["A", "B"])
    old = planner.decompose("q?", const('[{"q": "a?", "ref": "A"}]'))
    assert old == (["a?"], ["A"])
    assert planner.decompose("q?", const("nope")) == (["q?"], [""])
    assert planner.decompose(" ", const("x")) == ([], [])
