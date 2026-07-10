"""Test RetrievalAgent.run() op-routing with a fake substrate (no kernel/LLM).

Runnable script (PASS/FAIL prints). Verifies the op router, the token/limits
wiring into the injected LLM callable, and JSON output shapes.
"""
from __future__ import annotations

import json
import sys
import types

from agent import RetrievalAgent

FAILS: list[str] = []


def check(name: str, cond: bool) -> None:
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond:
        FAILS.append(name)


def task(obj: dict, token: str = "tok-123") -> types.SimpleNamespace:
    return types.SimpleNamespace(
        data=json.dumps(obj).encode("utf-8"), text="",
        session_token_id=token, deadline_remaining_ms=5000,
    )


a = RetrievalAgent(agent_id="retrieval_test")

# Fake substrate: records the token it was called with, returns canned planner JSON.
seen: dict = {}


def fake_generate(session_token_id: str, prompt: str, **kw) -> str:
    seen["token"] = session_token_id
    seen["max_tokens"] = kw.get("max_tokens")
    seen["temperature"] = kw.get("temperature")
    return json.dumps({"must_terms": ["Titan migration", "Bob Chen"], "phrases": ["Titan migration"]})


a.substrate = types.SimpleNamespace(generate=fake_generate)

# 1) plan_step → QuerySpec dict, token + temp=0 propagated to the LLM call
out = json.loads(a.run(task({"op": "plan_step", "query": "what did Alice's manager approve?"})))
check("plan_step must_terms", out.get("must_terms") == ["Titan migration", "Bob Chen"])
check("plan_step lexical_query built", '"Titan migration"' in out.get("lexical_query", ""))
check("token propagated to substrate", seen.get("token") == "tok-123")
check("temperature 0", seen.get("temperature") == 0.0)

# 2) decide_continue → fake LLM returns plan JSON (no decision) → stop_answer
out2 = json.loads(a.run(task({"op": "decide_continue", "query": "q", "chunks": ["x"]})))
check("decide_continue stops on no-decision", out2.get("decision") == "stop_answer")

# 3) synthesize → typed status (fake LLM returns plan JSON w/o status → defaults answer)
out3 = json.loads(a.run(task({"op": "synthesize", "query": "q", "chunks": ["chunk A"]})))
check("synthesize returns valid status", out3.get("status") in ("answer", "abstention", "clarification"))

# 4) unknown op → error payload (never crashes)
out4 = json.loads(a.run(task({"op": "bogus"})))
check("unknown op -> error", "error" in out4)

# 5) malformed request (bad JSON) → treated as empty op, error
bad = types.SimpleNamespace(data=b"not json", text="", session_token_id="t", deadline_remaining_ms=0)
out5 = json.loads(a.run(bad))
check("bad json -> error, no crash", "error" in out5)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {FAILS}")
    sys.exit(1)
print("\nall retrieval_agent checks passed")
