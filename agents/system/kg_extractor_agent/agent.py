"""kg_extractor agent (DeterministicAgent) — ADR-0053 D2 (revised) tiered KG extraction.

The system's write-time triplet extractor, with NO LLM. The kernel hands it a
batch of chunk texts (a Handoff) and it returns the per-chunk (h, r, t) triplets
(a Handoff), produced by the frozen tiered pipeline:

  * Tier 1 ``metadata``       — structural ``[date] Speaker:`` header parse.
  * Tier 2 ``spacy_patterns`` — dep-parse SVO + Hearst/appositive/possessive/acl/relcl.

It is a privileged system organ (``domain.IsSystemAgent``): invoked DIRECTLY by
the kernel on the ingest path, never auctioned, gatekept, or interviewed — the
same class as the pre-plan Scout. Being a ``DeterministicAgent`` it has no
``think()`` / LLM by construction.

Each triplet carries ``sources`` (which tiers produced it) and ``confidence``
(2=high when >=2 tiers agree, else 1=low) per ADR-0053 D2 (revised).
"""
from __future__ import annotations

import json
import re

from cambrian_agent_sdk import DeterministicAgent
from cambrian_agent_sdk._logging import configure_logging

from kg_extractors.common import Chunk
from kg_extractors.anchor_extractor import AnchorExtractor
from kg_extractors.metadata_extractor import MetadataExtractor
from kg_extractors.spacy_pattern_extractor import SpaCyPatternExtractor

AGENT_DESCRIPTION = (
    "Deterministic, no-LLM knowledge-graph extractor: turns a batch of chunk texts into "
    "per-chunk (h, r, t) triplets via metadata + spaCy dependency/pattern extraction. "
    "Kernel-invoked on the ingest path."
)


def _norm(s: str) -> str:
    return " ".join((s or "").strip().lower().split())


# "[2:24 pm on 14 August, 2023] Caroline: body..." -> group(1) = speaker
_PREFIX = re.compile(r"^\s*\[[^\]]*\]\s*([^:]{1,40}?):\s*", re.DOTALL)


def _speaker_of(text: str) -> str:
    m = _PREFIX.match(text or "")
    return m.group(1).strip() if m else ""


class KgExtractorAgent(DeterministicAgent):
    """No-LLM tiered triplet extractor. ``run`` handles one batch handoff."""

    def __init__(self, agent_id: str = "kg_extractor_agent", **kwargs) -> None:
        super().__init__(agent_id=agent_id, description=AGENT_DESCRIPTION, **kwargs)
        # Build the extractors ONCE — the spaCy model load is the expensive step;
        # amortise it across every handoff for the life of the process.
        self._metadata = MetadataExtractor()
        self._anchor = AnchorExtractor()  # deterministic structural-anchor tier (ADR-0053 D2)
        self._patterns = SpaCyPatternExtractor()  # raises ExtractorUnavailable if spaCy missing

    def run(self, task):
        texts, ids = self._parse_request(task)
        triplets_per_chunk = self._extract(texts, ids)
        return json.dumps({"triplets": triplets_per_chunk}).encode("utf-8")

    # ── internals ────────────────────────────────────────────────────────────

    @staticmethod
    def _parse_request(task) -> tuple[list[str], list[str]]:
        raw = task.data if getattr(task, "data", None) else (task.text or "").encode("utf-8")
        try:
            obj = json.loads(raw.decode("utf-8") if isinstance(raw, (bytes, bytearray)) else raw)
        except Exception:
            return [], []
        if not isinstance(obj, dict):
            return [], []
        texts = obj.get("texts") if isinstance(obj.get("texts"), list) else []
        ids = obj.get("ids") if isinstance(obj.get("ids"), list) else []
        texts = [str(t) for t in texts]
        ids = [str(i) for i in ids]
        return texts, ids

    def _extract(self, texts: list[str], ids: list[str]) -> list[list[dict]]:
        out: list[list[dict]] = [[] for _ in texts]
        if not texts:
            return out

        # One Chunk per text. chunk_id = positional index (the response is
        # positional); dia_id = the real document id so the metadata tier anchors
        # its temporal triplets to the chunk, not the index; speaker parsed from
        # the "[date] Speaker:" prefix so "spoke to"/"spoke at" resolve. A shared
        # session lets the metadata tier discover co-speakers across the batch.
        chunks = [
            Chunk(chunk_id=str(i), text=t, session_id="batch",
                  session_date="", speaker=_speaker_of(t),
                  dia_id=(ids[i] if i < len(ids) else ""), metadata={})
            for i, t in enumerate(texts)
        ]

        # Merge the two tiers, deduped per chunk on normalized (h, r, t); a triplet
        # seen from >=2 tiers is high-confidence, else low.
        # merged[idx][(h,r,t)] = {"triplet": {...}, "sources": set()}
        merged: list[dict] = [dict() for _ in texts]
        for name, ext in (("metadata", self._metadata), ("anchor", self._anchor), ("spacy_patterns", self._patterns)):
            try:
                res = ext.extract(chunks)
            except Exception:
                continue
            for tr in res.triplets:
                try:
                    idx = int(tr.chunk_id)
                except (TypeError, ValueError):
                    continue
                if idx < 0 or idx >= len(texts):
                    continue
                h, r, t = tr.subject.strip(), tr.relation.strip(), tr.object.strip()
                if not h or not r or not t:
                    continue
                key = (_norm(h), _norm(r), _norm(t))
                slot = merged[idx].get(key)
                if slot is None:
                    merged[idx][key] = {
                        "h": _norm(h), "r": r.lower(), "t": _norm(t),
                        "weight": float(getattr(tr, "weight", 1.0) or 1.0),
                        "sources": {name},
                    }
                else:
                    slot["sources"].add(name)

        for idx, slots in enumerate(merged):
            rows = []
            for s in slots.values():
                srcs = sorted(s["sources"])
                rows.append({
                    "h": s["h"], "r": s["r"], "t": s["t"], "weight": s["weight"],
                    "sources": srcs,
                    "confidence": 2 if len(srcs) >= 2 else 1,
                })
            out[idx] = rows
        return out


agent = KgExtractorAgent(agent_id="kg_extractor_agent")


if __name__ == "__main__":
    configure_logging(agent_id="kg_extractor_agent")
    agent.serve()
