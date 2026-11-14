"""RetrievalAgent.run() op routing over a fake substrate (no kernel, no LLM).

Verifies the op router, the C1 request each op sends through
``generate_request`` (system blocks, the user message, the response schema, the
lease token, temperature 0), the checked/unchecked decode switch, and the JSON
output shapes.
"""
from __future__ import annotations

import json
import types


from cambrian_agent_sdk import SchemaValidationFailed
from cambrian_agent_sdk.llm import GenerateResult

import planner
from agent import RetrievalAgent


def task(obj, token: str = "tok-123") -> types.SimpleNamespace:
    data = obj if isinstance(obj, bytes) else json.dumps(obj).encode("utf-8")
    return types.SimpleNamespace(data=data, text="", session_token_id=token, deadline_remaining_ms=5000)


class FakeSubstrate:
    """Records every generate_request call and answers from a script."""

    def __init__(self, text="", ignored=(), legacy=False, raise_exc=None):
        self.calls: list[dict] = []
        self.text, self.ignored, self.legacy, self.raise_exc = text, list(ignored), legacy, raise_exc

    def generate_request(self, session_token_id, **kw):
        self.calls.append(dict(kw, token=session_token_id))
        if self.raise_exc is not None:
            raise self.raise_exc
        text = self.text(kw) if callable(self.text) else self.text
        return GenerateResult(text=text, ignored=list(self.ignored), legacy_kernel=self.legacy)


def agent_with(sub: FakeSubstrate) -> RetrievalAgent:
    a = RetrievalAgent(agent_id="retrieval_test")
    a.substrate = sub
    return a


def test_plan_step_sends_c1_request_with_schema_and_lease():
    sub = FakeSubstrate(json.dumps({"must_terms": ["Titan migration", "Bob Chen"],
                                    "should_terms": [], "phrases": ["Titan migration"]}))
    out = json.loads(agent_with(sub).run(task({"op": "plan_step", "query": "what did Alice's manager approve?"})))
    assert out["must_terms"] == ["Titan migration", "Bob Chen"]
    assert '"Titan migration"' in out["lexical_query"]
    call = sub.calls[0]
    assert call["token"] == "tok-123"
    assert call["temperature"] == 0.0 and call["timeout_ms"] == 0
    assert call["cache_ttl"] == "short"
    assert call["system"][-1]["cache"] is True
    assert all(m["role"] != "system" for m in call["messages"])
    assert call["response_schema"]["name"] == "retrieval_plan_first"
    assert "<question>" in call["messages"][0]["content"]


def test_checked_reply_is_not_scraped_but_unchecked_one_is():
    prose = 'Sure! {"status": "answer", "text": "X"}'
    checked = json.loads(agent_with(FakeSubstrate(prose)).run(
        task({"op": "synthesize", "query": "q", "chunks": ["X"]})))
    assert checked["status"] == "abstention" and checked["failure"] == "unparseable_reply"
    for sub in (FakeSubstrate(prose, ignored=["response_schema"]), FakeSubstrate(prose, legacy=True)):
        out = json.loads(agent_with(sub).run(task({"op": "synthesize", "query": "q", "chunks": ["X"]})))
        assert out == {"status": "answer", "text": "X"}


def test_schema_validation_failure_is_an_explicit_abstention():
    sub = FakeSubstrate(raise_exc=SchemaValidationFailed("reply failed its schema", code="ABORTED"))
    out = json.loads(agent_with(sub).run(task({"op": "synthesize_cited", "query": "q", "chunks": ["c"]})))
    assert out["status"] == "abstention" and out["text"] == "not found in memory"


def test_decide_continue_and_unknown_and_malformed_ops():
    a = agent_with(FakeSubstrate('{"must_terms": []}'))
    assert json.loads(a.run(task({"op": "decide_continue", "query": "q", "chunks": ["x"]})))["decision"] == "stop_answer"
    assert "error" in json.loads(a.run(task({"op": "bogus"})))
    # Deleted ops (owner decisions 2026-11-15): IRCoT reason_step (ENG-1321) and the
    # never-called extract_provenance (ENG-641) are unknown ops now.
    assert "error" in json.loads(a.run(task({"op": "reason_step", "query": "q"})))
    assert "error" in json.loads(a.run(task({"op": "extract_provenance", "text": "x"})))
    assert "error" in json.loads(a.run(task(b"not json")))


def test_decompose_op_routes_switch_prompt_and_shape():
    def reply(kw):
        if kw["response_schema"]["name"] == "retrieval_decompose_routed":
            return '{"route": "direct", "sub_questions": []}'
        return '{"sub_questions": [{"q": "Which vendor did we pick?", "ref": ""}]}'

    sub = FakeSubstrate(reply)
    a = agent_with(sub)
    q = "Which vendor did we pick?"
    routed = json.loads(a.run(task({"op": "decompose", "query": q, "routes": True})))
    assert routed == {"route": "direct", "sub_questions": [q], "refs": [""]}
    assert sub.calls[-1]["system"][0]["text"] == planner.LIBRARY.get("retrieval_decompose_routed").body
    old = json.loads(a.run(task({"op": "decompose", "query": q})))
    assert "route" not in old and old["sub_questions"] == [q]
    assert sub.calls[-1]["system"][0]["text"] == planner.LIBRARY.get("retrieval_decompose").body
    assert "route" not in json.loads(a.run(task({"op": "decompose", "query": q, "routes": "yes"})))
