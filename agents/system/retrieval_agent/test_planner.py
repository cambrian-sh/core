"""Test the retrieval planner logic (plan_step + QuerySpec compilation).

Runnable as a plain script (like docling_agent/test_agent.py): prints
PASS/FAIL per check and exits non-zero if any fail. Uses a fake LLM, so no
model / kernel needed.
"""
from __future__ import annotations

import json
import sys

from planner import QuerySpec, extract_json, plan_step

FAILS: list[str] = []


def check(name: str, cond: bool) -> None:
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond:
        FAILS.append(name)


# 1) extract_json — fenced, braced, none
check("extract fenced", extract_json('x ```json\n{"a":1}\n``` y') == {"a": 1})
check("extract braced", extract_json('noise {"b":2} tail') == {"b": 2})
check("extract none", extract_json("no json") is None)

# 2) QuerySpec.to_lexical_query — phrases quoted + must + should, dedup
spec = QuerySpec(
    nl_intent="what did the rose say in chapter 3 scene 2",
    must_terms=["chapter 3", "scene 2"],
    should_terms=["rose"],
    phrases=["scene 2"],
)
lex = spec.to_lexical_query()
check("phrase quoted first", lex.startswith('"scene 2"'))
check("must terms present", "chapter 3" in lex)
check("should term present", "rose" in lex)

# dedup collapses a repeated token to one occurrence
dup = QuerySpec(nl_intent="x", must_terms=["alpha", "alpha", "beta"])
check("dedup repeated token", dup.to_lexical_query() == "alpha beta")

# 3) empty spec falls back to the raw intent (loop never emits empty search)
empty = QuerySpec(nl_intent="fallback query text")
check("empty falls back to intent", empty.to_lexical_query() == "fallback query text")


# 4) plan_step with a fake LLM that selects discriminative terms
def fake_good(prompt: str) -> str:
    assert "query planner" in prompt
    return "```json\n" + json.dumps({
        "must_terms": ["Titan migration", "Bob Chen"],
        "should_terms": ["approved"],
        "phrases": ["Titan migration"],
    }) + "\n```"


got = plan_step("What did the person Alice reports to approve?", fake_good)
check("plan_step parses must_terms", got.must_terms == ["Titan migration", "Bob Chen"])
check("plan_step compiles discriminative query",
      '"Titan migration"' in got.to_lexical_query() and "Bob Chen" in got.to_lexical_query())


# 5) plan_step with garbage LLM output → empty terms → fallback to raw query
def fake_bad(prompt: str) -> str:
    return "I cannot help with that."


bad = plan_step("some user question", fake_bad)
check("garbage -> no terms", bad.must_terms == [] and bad.phrases == [])
check("garbage -> falls back to raw query", bad.to_lexical_query() == "some user question")


# 6) plan_step with a scratchpad (later hop) centers on the bridge
def fake_hop(prompt: str) -> str:
    assert "later hop" in prompt and "Bob Chen" in prompt  # hop prompt used
    return json.dumps({"must_terms": ["Bob Chen", "approved"], "phrases": []})


hop = plan_step("What did the person Alice reports to approve?", fake_hop, scratchpad="Bob Chen")
check("hop plan centers on bridge", "Bob Chen" in hop.to_lexical_query())
# later-hop fallback is the bridge, not the raw question
hop_bad = plan_step("orig question", fake_bad, scratchpad="Bob Chen")
check("hop garbage -> falls back to bridge", hop_bad.to_lexical_query() == "Bob Chen")


# 7) decide_continue: continue with bridge / stop / garbage
from planner import decide_continue


def fake_continue(prompt: str) -> str:
    assert "multi-hop retrieval loop" in prompt
    return json.dumps({"decision": "continue", "bridge": "Bob Chen"})


d1 = decide_continue({"query": "q", "chunks": ["Alice reports to Bob Chen"]}, fake_continue)
check("decide continue -> bridge", d1 == {"decision": "continue", "bridge": "Bob Chen"})

d2 = decide_continue({"query": "q", "chunks": ["answer here"]}, lambda p: '{"decision":"stop_answer"}')
check("decide stop", d2["decision"] == "stop_answer")

d3 = decide_continue({"query": "q", "chunks": []}, fake_bad)  # garbage -> stop (fail-safe)
check("decide garbage -> stop", d3["decision"] == "stop_answer")

d4 = decide_continue({"query": "q", "chunks": ["x"]}, lambda p: '{"decision":"continue"}')  # no bridge
check("continue without bridge -> stop", d4["decision"] == "stop_answer")


# 8) synthesize: typed three-way + fail-safe default
from planner import synthesize

s_ans = synthesize({"query": "q", "chunks": ["the answer is X"]},
                   lambda p: '{"status":"answer","text":"X"}')
check("synthesize answer", s_ans == {"status": "answer", "text": "X"})

s_abs = synthesize({"query": "q", "chunks": ["unrelated"]},
                   lambda p: '{"status":"abstention","text":"not found in memory"}')
check("synthesize abstention", s_abs["status"] == "abstention")

s_clr = synthesize({"query": "what did the manager approve?", "chunks": ["m1", "m2"]},
                   lambda p: '{"status":"clarification","text":"which manager?"}')
check("synthesize clarification", s_clr["status"] == "clarification")

s_bad = synthesize({"query": "q", "chunks": []}, fake_bad)  # garbage -> default answer
check("synthesize garbage -> answer (fail-safe)", s_bad["status"] == "answer")

s_unknown = synthesize({"query": "q", "chunks": []}, lambda p: '{"status":"maybe"}')
check("synthesize unknown status -> answer", s_unknown["status"] == "answer")


# 9) extract_provenance: schema-bounded + fail-safe
from planner import extract_provenance

pv = extract_provenance(
    "Dana rotated the API keys on June 12 2025.",
    lambda p: '{"valid_time":"2025-06-12","actors":["Dana"],"source_type":"system_event"}',
)
check("provenance valid_time", pv["valid_time"] == "2025-06-12")
check("provenance actors", pv["actors"] == ["Dana"])
check("provenance source_type", pv["source_type"] == "system_event")
check("provenance origin=inferred", pv["origin"] == "inferred")

pv_bad_type = extract_provenance("x", lambda p: '{"source_type":"bogus","actors":["A"]}')
check("provenance unknown source_type -> ''", pv_bad_type["source_type"] == "")

pv_garbage = extract_provenance("x", fake_bad)  # no JSON -> empty fields, no crash
check("provenance garbage -> empty", pv_garbage["valid_time"] == "" and pv_garbage["actors"] == [])


if FAILS:
    print(f"\n{len(FAILS)} FAILED: {FAILS}")
    sys.exit(1)
print("\nall planner checks passed")
