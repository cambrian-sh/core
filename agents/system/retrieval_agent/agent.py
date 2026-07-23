"""retrieval_agent (CognitiveAgent) — the LLM steps of the agentic retrieval loop.

The "thin stateless Python endpoint" from AGENTIC_RETRIEVAL_SPEC §2.1. The Go
loop (QueryService.AgenticQuery) owns control + the retrieval tiers and calls
this agent once per step via ``Auctioneer.CallAgent`` with a JSON Handoff
``{op, ...}``. This agent is a pure function of its input: it holds NO loop
state (Go passes it in each call), so it is trivially restartable + testable.

Ops (dispatched in :meth:`run`):
  * ``plan_step``      → {query} → the lexical QuerySpec (Phase 2a).
  * ``decide_continue``→ {state} → the stop/continue decision (stub → later phase).
  * ``synthesize``     → {chunks} → the typed answer (stub → later phase).

LLM access: a kernel-invoked agent receives a valid ``session_token_id`` on the
task (from the Handoff), so ``self.substrate.generate(token, prompt)`` reaches
the managed model proxy. This is why the endpoint works under kernel invocation
even though a bare external ``generate_stream`` (no session) is rejected.

Why a CognitiveAgent (not Deterministic): it needs ``self.substrate`` (the LLM
gateway), which the cognitive trait binds in ``serve()``. We override ``run``
to dispatch ops directly rather than drive the ReAct loop — the loop's control
lives in Go, not here.
"""
from __future__ import annotations

import json
import os

from cambrian_agent_sdk import CognitiveAgent
from cambrian_agent_sdk._logging import configure_logging

import planner

AGENT_DESCRIPTION = (
    "Agentic retrieval LLM endpoint: plan_step (lexical query planning), "
    "decide_continue, and synthesize. Kernel-invoked once per loop step "
    "(AGENTIC_RETRIEVAL_SPEC §2.1)."
)

# temp=0 for benchmark determinism (spec §2.1); tokens bounded per op.
_TEMPERATURE = 0.0
_PLAN_MAX_TOKENS = int(os.environ.get("RETRIEVAL_PLAN_MAX_TOKENS", "400"))
_SYNTH_MAX_TOKENS = int(os.environ.get("RETRIEVAL_SYNTH_MAX_TOKENS", "1024"))


class RetrievalAgent(CognitiveAgent):
    role = "A precise retrieval query planner and answer synthesizer."

    def __init__(self, agent_id: str = "retrieval_agent", **kwargs) -> None:
        super().__init__(agent_id=agent_id, description=AGENT_DESCRIPTION, **kwargs)

    def run(self, task):
        obj = self._parse_request(task)
        op = str(obj.get("op", ""))
        if op == "plan_step":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            history = obj.get("history") if isinstance(obj.get("history"), list) else []
            spec = planner.plan_step(
                str(obj.get("query", "")), llm,
                scratchpad=str(obj.get("scratchpad", "")),
                history=[str(h) for h in history],
                hop_index=int(obj.get("hop", 0) or 0),
            )
            return self._ok(spec.to_dict())
        if op == "decide_continue":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            history = obj.get("history") if isinstance(obj.get("history"), list) else []
            state = {"query": str(obj.get("query", "")), "chunks": chunks, "history": history}
            return self._ok(planner.decide_continue(state, llm))
        if op == "synthesize":
            llm = self._make_llm(task, _SYNTH_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            state = {"query": str(obj.get("query", "")), "chunks": chunks}
            return self._ok(planner.synthesize(state, llm))
        if op == "synthesize_cited":
            # ADR-0081: grounded answer with inline [n] citations. Distinct op so
            # the benchmark synthesize (whose answer text the scorer substring-matches)
            # is never polluted with citation markers.
            llm = self._make_llm(task, _SYNTH_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            state = {"query": str(obj.get("query", "")), "chunks": chunks}
            return self._ok(planner.synthesize_cited(state, llm))
        if op == "reason_step":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            cot = obj.get("cot") if isinstance(obj.get("cot"), list) else []
            state = {"query": str(obj.get("query", "")), "chunks": chunks, "cot": cot}
            return self._ok(planner.reason_step(state, llm))
        if op == "decompose":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            subqs, refs = planner.decompose(str(obj.get("query", "")), llm)
            return self._ok({"sub_questions": subqs, "refs": refs})
        if op == "answer_subq":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            ans = planner.answer_subquestion(str(obj.get("subq", "")), chunks, llm)
            return self._ok({"answer": ans})
        if op == "hyde":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            return self._ok({"passage": planner.hyde_passage(str(obj.get("query", "")), llm)})
        if op == "extract_provenance":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            return self._ok(
                planner.extract_provenance(
                    str(obj.get("text", "")), llm, hint=str(obj.get("hint", ""))
                )
            )
        return self._ok({"error": f"unknown op {op!r}"})

    # ── internals ────────────────────────────────────────────────────────────

    def _make_llm(self, task, max_tokens: int):
        """Build the injected LLM callable bound to this invocation's session
        token + remaining deadline (so a slow model can't leak a thread past
        the Go step deadline)."""
        token = str(getattr(task, "session_token_id", "") or "")
        timeout_ms = int(getattr(task, "deadline_remaining_ms", 0) or 0)

        def _llm(prompt: str) -> str:
            return self.substrate.generate(
                session_token_id=token,
                prompt=prompt,
                max_tokens=max_tokens,
                temperature=_TEMPERATURE,
                timeout_ms=timeout_ms,
            )

        return _llm

    @staticmethod
    def _parse_request(task) -> dict:
        raw = task.data if getattr(task, "data", None) else (getattr(task, "text", "") or "").encode("utf-8")
        try:
            obj = json.loads(raw.decode("utf-8") if isinstance(raw, (bytes, bytearray)) else raw)
        except Exception:
            return {}
        return obj if isinstance(obj, dict) else {}

    @staticmethod
    def _ok(payload: dict) -> bytes:
        return json.dumps(payload).encode("utf-8")


agent = RetrievalAgent(agent_id="retrieval_agent")


if __name__ == "__main__":  # pragma: no cover
    configure_logging(agent_id="retrieval_agent")
    agent.serve()
