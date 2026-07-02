"""reranker agent (DeterministicAgent) — ADR-0054 Stage B cross-encoder rerank.

The system's query-time relevance oracle. The kernel hands it a Handoff of
``{query, documents[]}`` (the top-K Stage-A candidates) and it returns
``{scores[]}`` — one bge cross-encoder relevance score per document, in input
order. The Go side blends each score with the Stage-A score via
``FinalScore = w_bge * bge + (1 - w_bge) * stageA`` (internal/memory/blend.go).

It is a privileged system organ (``domain.IsSystemAgent``): invoked DIRECTLY by
the kernel on the recall path via the Auctioneer (no auction / gatekeeper /
interview), the same class as the kg_extractor and the pre-plan Scout. Being a
``DeterministicAgent`` it has no ``think()`` / generative LLM by construction —
the cross-encoder is a scoring model, not a generator.

Why an agent (not a standalone HTTP sidecar): the agent runtime keeps the
process WARM (``serve()`` blocks for the life of the process), so the
cross-encoder — whose load is the expensive step — is built ONCE in __init__ and
amortised across every recall, exactly like kg_extractor amortises the spaCy
load. A subprocess-per-call shape would reload the model each query and is a
non-starter on the hot read path.

Model (RERANK_MODEL env, default ``BAAI/bge-reranker-base``):
  * CPU edge production (default) → bge-reranker-base or bge-reranker-v2-m3
  * validation / ceiling runs     → RERANK_MODEL=BAAI/bge-reranker-large
The default is the DEPLOYABLE model (CPU edge), not the experiment: large is the
opt-in ceiling override, so a fresh kernel boot doesn't stall on a 1.1GB download
before the agent can register. The wire contract is identical across models; the
choice is config, not code
(Zero-Hardcode). bge cross-encoders emit a single raw relevance LOGIT per pair;
this agent returns those RAW logits untouched. The Go side min-max normalizes
them across the candidate set before blending (the same treatment PageRank gets
in query.go) — that preserves the true logit gaps the model expresses, whereas a
sigmoid would crush all-negative logits (relevant passages included) toward 0 and
destroy the discrimination. NB: sentence-transformers' CrossEncoder.predict
applies its own default activation for a 1-logit head; we disable it with
``activation_fn=torch.nn.Identity()`` so we get the raw logits, not a pre-squashed
score that we'd then have to un-transform.
"""
from __future__ import annotations

import json
import os

from cambrian_agent_sdk import DeterministicAgent
from cambrian_agent_sdk._logging import configure_logging

RERANK_MODEL = os.environ.get("RERANK_MODEL", "BAAI/bge-reranker-base")
RERANK_MAXLEN = int(os.environ.get("RERANK_MAXLEN", "512"))

AGENT_DESCRIPTION = (
    "Deterministic bge cross-encoder reranker: scores (query, document) pairs for "
    "relevance and returns one score per document. Kernel-invoked on the recall path "
    "(ADR-0054 Stage B)."
)


class RerankerAgent(DeterministicAgent):
    """Warm cross-encoder. ``run`` scores one {query, documents} handoff."""

    def __init__(self, agent_id: str = "reranker_agent", **kwargs) -> None:
        super().__init__(agent_id=agent_id, description=AGENT_DESCRIPTION, **kwargs)
        # Load the cross-encoder ONCE — the model load is the expensive step;
        # amortise it across every recall for the life of the process.
        from sentence_transformers import CrossEncoder

        self._model = CrossEncoder(RERANK_MODEL, max_length=RERANK_MAXLEN)

    def run(self, task):
        query, documents = self._parse_request(task)
        scores = self._score(query, documents)
        return json.dumps({"scores": scores}).encode("utf-8")

    # ── internals ────────────────────────────────────────────────────────────

    @staticmethod
    def _parse_request(task) -> tuple[str, list[str]]:
        raw = task.data if getattr(task, "data", None) else (task.text or "").encode("utf-8")
        try:
            obj = json.loads(raw.decode("utf-8") if isinstance(raw, (bytes, bytearray)) else raw)
        except Exception:
            return "", []
        if not isinstance(obj, dict):
            return "", []
        query = str(obj.get("query", "") or "")
        docs = obj.get("documents") if isinstance(obj.get("documents"), list) else []
        return query, [str(d) for d in docs]

    def _score(self, query: str, documents: list[str]) -> list[float]:
        if not documents:
            return []
        import torch

        pairs = [(query, d) for d in documents]
        # One batched forward over all K pairs — the whole point of K being bounded.
        # activation_fn=Identity ⇒ RAW logits (predict's default sigmoid for a
        # 1-logit head is exactly what we must NOT apply here; the Go side
        # min-max normalizes the raw logits across the candidate set).
        logits = self._model.predict(
            pairs,
            activation_fn=torch.nn.Identity(),
            convert_to_numpy=True,
            show_progress_bar=False,
        )
        return [float(x) for x in logits]


agent = RerankerAgent(agent_id="reranker_agent")


if __name__ == "__main__":
    configure_logging(agent_id="reranker_agent")
    agent.serve()
