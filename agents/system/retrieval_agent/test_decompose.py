"""Test the decompose op: the legacy plan and the ADR-0135 routed plan.

Runnable as a plain script like test_planner.py (PASS/FAIL per check, non-zero
exit on failure); also collected by pytest (test_all). Fake LLMs only — no model,
no kernel.
"""
from __future__ import annotations

import json
import sys
import types

import planner
from agent import RetrievalAgent

FAILS: list[str] = []


def check(name: str, cond: bool) -> None:
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond:
        FAILS.append(name)


def const(text: str):
    return lambda prompt: text


def run_checks() -> None:
    # 1) The routed prompt formats (every literal brace escaped) and carries the
    #    route definitions plus the memory-style examples.
    p = planner.DECOMPOSE_ROUTE_PROMPT.format(query="Q?")
    check("routed prompt formats", p.endswith("Question: Q?"))
    for needle in ('"direct"', '"facets"', '"chain"', "Where do I take yoga classes?",
                   "How many graduation ceremonies have I attended in the past three months?",
                   "Where does the friend I went hiking with work?", "{1}"):
        check(f"routed prompt mentions {needle!r}", needle in p)
    check("legacy prompt has no route", '"route"' not in planner.DECOMPOSE_PROMPT)

    # 2) Well-formed object per route.
    direct = planner.decompose_routed(
        "Where do I take yoga classes?", const('{"route": "direct", "sub_questions": []}'))
    check("direct route", direct["route"] == "direct")
    check("direct carries the question as its one sub-question",
          direct["sub_questions"] == ["Where do I take yoga classes?"] and direct["refs"] == [""])

    facets = planner.decompose_routed("How many?", const(json.dumps({
        "route": "facets",
        "sub_questions": [{"q": "When did I buy the blender?", "ref": "blender date"},
                          {"q": "When did I buy the toaster?", "ref": "toaster date"}]})))
    check("facets route", facets["route"] == "facets")
    check("facets sub-questions", facets["sub_questions"] == ["When did I buy the blender?", "When did I buy the toaster?"])
    check("facets refs", facets["refs"] == ["blender date", "toaster date"])

    chain = planner.decompose_routed("Where does the friend I hiked with work?", const(
        "```json\n" + json.dumps({"route": "Chain", "sub_questions": [
            {"q": "Which friend did I go hiking with?", "ref": "the friend"},
            {"q": "Where does {1} work?", "ref": "the workplace of {1}"}]}) + "\n```"))
    check("chain route lower-cased, fenced output parsed", chain["route"] == "chain")
    check("chain keeps placeholders", chain["sub_questions"][1] == "Where does {1} work?")

    # 3) Defensive parsing.
    legacy = planner.decompose_routed("q?", const('[{"q": "a?", "ref": "A"}, {"q": "b of {1}?", "ref": "B"}]'))
    check("route-less legacy list -> route '' with its plan",
          legacy["route"] == "" and legacy["sub_questions"] == ["a?", "b of {1}?"])
    single_legacy = planner.decompose_routed("q?", const('[{"q": "a?", "ref": "A"}]'))
    check("one-element legacy list is still a plan", single_legacy["sub_questions"] == ["a?"])
    garbage = planner.decompose_routed("Where do I live?", const("I cannot help with that."))
    check("garbage -> route '' + the question (fail-safe echo)",
          garbage == {"route": "", "sub_questions": ["Where do I live?"], "refs": [""]})
    missing = planner.decompose_routed("q?", const('{"sub_questions": [{"q": "a?"}]}'))
    check("object without route -> route ''", missing["route"] == "" and missing["sub_questions"] == ["a?"])
    unknown = planner.decompose_routed("q?", const('{"route": "Parallel", "sub_questions": ["a?", "b?"]}'))
    check("unknown route passed through for the kernel to judge", unknown["route"] == "parallel")
    check("bare-string sub-questions tolerated", unknown["sub_questions"] == ["a?", "b?"] and unknown["refs"] == ["", ""])
    odd = planner.decompose_routed("q?", const('{"route": 7, "sub_questions": "a?"}'))
    check("non-string route and non-list sub_questions -> fail-safe",
          odd == {"route": "", "sub_questions": ["q?"], "refs": [""]})
    junk_items = planner.decompose_routed("q?", const('{"route": "facets", "sub_questions": [null, {"q": ""}, ["x"], {"q": "a?"}]}'))
    check("junk items skipped", junk_items["sub_questions"] == ["a?"])
    many = planner.decompose_routed("q?", const(json.dumps(
        {"route": "facets", "sub_questions": [{"q": f"s{i}?"} for i in range(9)]})))
    check("capped at max_subqs", len(many["sub_questions"]) == 4 and len(many["refs"]) == 4)
    check("empty question -> empty plan", planner.decompose_routed("  ", const("x"))["sub_questions"] == [])

    # 4) The op: routes=true -> routed response; absent/false -> legacy shape and prompt.
    a = RetrievalAgent(agent_id="retrieval_decompose_test")
    prompts: list[str] = []

    def gen(session_token_id: str, prompt: str, **kw) -> str:
        prompts.append(prompt)
        if '"route"' in prompt:
            return '{"route": "direct", "sub_questions": []}'
        return '[{"q": "Where do I take yoga classes?", "ref": ""}]'

    a.substrate = types.SimpleNamespace(generate=gen)

    def task(obj: dict) -> types.SimpleNamespace:
        return types.SimpleNamespace(data=json.dumps(obj).encode("utf-8"), text="",
                                     session_token_id="tok", deadline_remaining_ms=0)

    q = "Where do I take yoga classes?"
    routed = json.loads(a.run(task({"op": "decompose", "query": q, "routes": True})))
    check("op routes=true -> route in response", routed.get("route") == "direct" and routed.get("sub_questions") == [q])
    check("op routes=true used the routed prompt", '"route"' in prompts[-1])
    old = json.loads(a.run(task({"op": "decompose", "query": q})))
    check("op without routes -> legacy shape (no route key)", "route" not in old and old.get("sub_questions") == [q])
    check("op without routes used the legacy prompt", prompts[-1] == planner.DECOMPOSE_PROMPT.format(query=q))
    off = json.loads(a.run(task({"op": "decompose", "query": q, "routes": "yes"})))
    check("op routes not exactly true -> legacy", "route" not in off)


def test_all() -> None:
    FAILS.clear()
    run_checks()
    assert not FAILS, FAILS


if __name__ == "__main__":
    run_checks()
    if FAILS:
        print(f"\n{len(FAILS)} FAILED: {FAILS}")
        sys.exit(1)
    print("\nall decompose checks passed")
