"""retrieval_agent (CognitiveAgent) — the LLM steps of the agentic retrieval loop.

The "thin stateless Python endpoint" from AGENTIC_RETRIEVAL_SPEC §2.1. The Go
loop (QueryService.AgenticQuery) owns control + the retrieval tiers and calls
this agent once per step via ``agentplane.Transport.CallAgent`` with a JSON Handoff
``{op, ...}``. This agent is a pure function of its input: it holds NO loop
state (Go passes it in each call), so it is trivially restartable + testable.

Ops (dispatched in :meth:`run`):
  * ``plan_step``         → {query[, scratchpad, history, hop]} → the lexical QuerySpec.
  * ``decide_continue``   → {query, chunks, history} → the stop/continue decision.
  * ``synthesize``        → {query, chunks} → the typed answer {status, text[, failure]}.
  * ``synthesize_cited``  → the same with inline [n] citations (ADR-0081).
  * ``decompose``         → {query[, routes]} → {sub_questions, refs}; with
    ``"routes": true`` (ADR-0135) also the planner's ``route`` (direct|facets|chain).
  * ``answer_subq``       → {subq, chunks} → {answer}.

LLM access: a kernel-invoked agent receives a valid ``session_token_id`` on the
task (from the Handoff), so ``self.substrate.generate_request(token, ...)``
reaches the managed model proxy. Each op sends the ADR-0139 C1 shape built by
``planner``: library system blocks (cached), one user message with the tagged
data, and the reply's JSON schema. This is why the endpoint works under kernel invocation
even though a bare external ``generate_stream`` (no session) is rejected.

Why a CognitiveAgent (not Deterministic): it needs ``self.substrate`` (the LLM
gateway), which the cognitive trait binds in ``serve()``. We override ``run``
to dispatch ops directly rather than drive the ReAct loop — the loop's control
lives in Go, not here.
"""
from __future__ import annotations

import json
import os

from cambrian_agent_sdk import CognitiveAgent, SchemaValidationFailed
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

# Experimental, default-off: route ONLY the decompose op to a local ollama model
# (the SFT-trained planner). Other ops keep the kernel's managed proxy — the
# trained model is a query formulator, not an extractor/synthesizer. Proper
# per-step model routing needs a GenerateOptions model field (contract change);
# this env hook exists so the A/B can run without that.
_DECOMPOSE_OLLAMA_MODEL = os.environ.get("RETRIEVAL_DECOMPOSE_OLLAMA_MODEL", "")
_OLLAMA_URL = os.environ.get("RETRIEVAL_OLLAMA_URL", "http://127.0.0.1:11434")


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
        if op == "decompose":
            if _DECOMPOSE_OLLAMA_MODEL:
                llm = self._make_ollama_llm(_PLAN_MAX_TOKENS)
            else:
                llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            if obj.get("routes") is True:
                # ADR-0135: the kernel executes routes and asked for one.
                return self._ok(planner.decompose_routed(str(obj.get("query", "")), llm))
            # Every other caller (flag off, older kernels): today's prompt and shape.
            subqs, refs = planner.decompose(str(obj.get("query", "")), llm)
            return self._ok({"sub_questions": subqs, "refs": refs})
        if op == "answer_subq":
            llm = self._make_llm(task, _PLAN_MAX_TOKENS)
            chunks = obj.get("chunks") if isinstance(obj.get("chunks"), list) else []
            ans = planner.answer_subquestion(str(obj.get("subq", "")), chunks, llm)
            return self._ok({"answer": ans})
        return self._ok({"error": f"unknown op {op!r}"})

    # ── internals ────────────────────────────────────────────────────────────

    def _make_llm(self, task, max_tokens: int):
        """Build the injected LLM callable bound to this invocation's session token.

        Sends the planner's structured request (ADR-0139 C1) and reports whether
        the reply's schema was checked: only when the kernel neither ignored
        ``response_schema`` nor predates the structured fields is the text JSON as
        is; otherwise the planner decodes it tolerantly. A reply that still failed
        its schema after the kernel's repair turn (``SchemaValidationFailed``) is
        an unreadable answer, handed to the op's own fail-safe as an empty,
        checked reply. Every other error propagates, as before: the kernel's
        dispatcher fails open on it.
        """
        token = str(getattr(task, "session_token_id", "") or "")
        # Pass NO client-side gRPC deadline (timeout_ms=0 ⇒ None), matching react.py's
        # deliberate choice for an agent's own LLM call. The former code used
        # task.deadline_remaining_ms as the timeout, but the SDK computes that from
        # context.time_remaining(); with no inbound deadline that returns ~infinity and
        # int(remaining*1000) overflows to a nonsense value (observed 9.2e21), which the gRPC
        # client turns into an INSTANT DEADLINE_EXCEEDED — the exact failure that killed every
        # decompose. The kernel's provider timeout + the session-token TTL still bound the call.
        def _llm(req: planner.LLMRequest) -> planner.Reply:
            try:
                res = self.substrate.generate_request(
                    token,
                    system=req.system,
                    messages=req.messages,
                    response_schema=req.response_schema,
                    # One retrieval chain is machine-paced: the short cache window.
                    cache_ttl="short",
                    max_tokens=max_tokens,
                    temperature=_TEMPERATURE,
                    timeout_ms=0,
                )
            except SchemaValidationFailed:
                return planner.Reply(text="", schema_checked=True)
            ignored = set(getattr(res, "ignored", None) or [])
            checked = (req.response_schema is not None
                       and not getattr(res, "legacy_kernel", False)
                       and "response_schema" not in ignored)
            return planner.Reply(text=str(getattr(res, "text", "") or ""), schema_checked=checked)

        return _llm

    @staticmethod
    def _make_ollama_llm(max_tokens: int):
        """Direct local-ollama backend for the decompose op (env-gated above). It
        has no system field and no schema check: the request is flattened and the
        reply decoded tolerantly."""
        import urllib.request

        def _llm(req: planner.LLMRequest) -> planner.Reply:
            body = {
                "model": _DECOMPOSE_OLLAMA_MODEL,
                "prompt": req.flatten(),
                "stream": False,
                "options": {"num_predict": max_tokens, "temperature": _TEMPERATURE},
            }
            http_req = urllib.request.Request(
                _OLLAMA_URL + "/api/generate",
                data=json.dumps(body).encode("utf-8"),
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(http_req, timeout=120) as resp:
                return planner.Reply(text=str(json.loads(resp.read())["response"]))

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
    import logging
    logging.getLogger("cambrian.retrieval_agent").info(
        "retrieval_agent prompt versions %s", planner.prompt_versions())
    agent.serve()
