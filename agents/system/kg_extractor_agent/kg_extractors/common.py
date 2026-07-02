"""Common data structures, LoCoMo loader, and the Extractor protocol.

The package is structured around three types:

* ``Chunk`` — a unit of text (typically one conversation turn with metadata).
* ``Triplet`` — an extracted ``(subject, relation, object[, weight])`` fact.
* ``Graph`` — a simple node/edge view over the triplets; built for inspection
  and (later) for routing during retrieval.

The evaluation harness (``evaluator.py``) consumes the *same* types
regardless of which extractor produced them, so a new extractor only needs
to honour the protocol.
"""
from __future__ import annotations

import json
import re
import time
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Protocol, Sequence, Tuple

# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

# Per the LLM-prompt used by Cambrian's ``EdgeExtractor`` (internal/memory/edge_extractor.go)
# the entity universe is mapped to 4 meta-kinds at extraction time. The rule
# extractors below emit the same meta-kind on the subject / object so the
# comparison is schema-aligned.
META_KIND_NAMED = "named"     # proper nouns: people, places, orgs, products, characters
META_KIND_LOCATED = "located"  # paths, URLs, endpoints, file-system entries
META_KIND_VALUED = "valued"   # versions, dates, IDs, counts, ticket numbers
META_KIND_CONCEPT = "concept"  # abstract: topics, methods, ideas, categories, skills
ALL_META_KINDS = (META_KIND_NAMED, META_KIND_LOCATED, META_KIND_VALUED, META_KIND_CONCEPT)

# Cambrian's document-edge vocabulary (ADR-0017) — typed predicates plus the
# open "extracted" verb phrases from ADR-0053 chunk_triplets.
TYPED_PREDICATES = (
    "closes",
    "specifies",
    "contradicts",
    "discussed_in",
    "follows",
    "engaged",
    "co_activated",
    "extracted",
)


@dataclass(frozen=True)
class Chunk:
    """One unit of text + provenance.

    For LoCoMo this is one conversation turn, but the same shape fits any
    text source (a file, a tool output, an episodic-memory snapshot).
    """

    chunk_id: str
    text: str
    session_id: str
    session_date: str
    speaker: str
    dia_id: str
    metadata: Dict[str, Any] = field(default_factory=dict)

    def render(self) -> str:
        """The text as it was ingested into LTM (with the date prefix)."""
        return render_chunk(self)


@dataclass(frozen=True)
class Triplet:
    """One extracted (h, r, t) fact.

    ``weight`` is the extractor's own confidence in [0, 1] — meaningful for
    the LLM and T5 extractors, less so for the deterministic ones (they
    always emit 1.0). ``source`` is the extractor name, so the evaluator can
    attribute each triplet when an ensemble is tested later.
    """

    chunk_id: str
    subject: str
    relation: str
    object: str
    subject_kind: str = META_KIND_CONCEPT
    object_kind: str = META_KIND_CONCEPT
    weight: float = 1.0
    source: str = ""
    extra: Dict[str, Any] = field(default_factory=dict)


@dataclass
class Graph:
    """A tiny node/edge view over the triplets.

    This is *only* for inspection + per-QA evaluation. The Cambrian kernel
    does the real storage in ``chunk_triplets`` / ``document_edges`` (Go /
    PostgreSQL). Keeping a parallel in-process graph lets the notebook draw
    small subgraphs, run BFS, and count per-evidence recall without a live
    database.
    """

    nodes: Dict[str, Dict[str, Any]] = field(default_factory=dict)
    edges: List[Triplet] = field(default_factory=list)

    def add_triplet(self, t: Triplet) -> None:
        for node, kind in ((t.subject, t.subject_kind), (t.object, t.object_kind)):
            if node not in self.nodes:
                self.nodes[node] = {"kind": kind, "count": 0, "weight_sum": 0.0}
            self.nodes[node]["count"] += 1
            self.nodes[node]["weight_sum"] += t.weight
        self.edges.append(t)

    def node_count(self) -> int:
        return len(self.nodes)

    def edge_count(self) -> int:
        return len(self.edges)

    def neighbours(self, node: str, depth: int = 1) -> List[str]:
        """Bounded BFS for the per-QA recall check (depth=1 by default)."""
        if depth <= 0:
            return []
        visited, queue, frontier = {node}, [node], {node}
        for _ in range(depth):
            next_frontier: set = set()
            for u in frontier:
                for e in self.edges:
                    if e.subject == u and e.object not in visited:
                        next_frontier.add(e.object)
                    elif e.object == u and e.subject not in visited:
                        next_frontier.add(e.subject)
            visited.update(next_frontier)
            frontier = next_frontier
        return sorted(visited - {node})


@dataclass
class ExtractionStats:
    """Per-extractor instrumentation, emitted by every run."""

    extractor: str
    num_chunks: int = 0
    num_triplets: int = 0
    num_entities: int = 0
    elapsed_ms: float = 0.0
    peak_memory_mb: float = 0.0
    notes: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ExtractionResult:
    """The output of an extractor + its instrumentation."""

    chunks: List[Chunk]
    triplets: List[Triplet]
    graph: Graph
    stats: ExtractionStats


class ExtractorUnavailable(RuntimeError):
    """Raised by an extractor when a third-party dep is missing or a network
    call fails. The notebook catches this and reports the row as ``-`` in
    the comparison table."""


# ---------------------------------------------------------------------------
# Extractor protocol
# ---------------------------------------------------------------------------

class Extractor(Protocol):
    """Every extractor implements this protocol."""

    name: str

    def extract(self, chunks: Sequence[Chunk]) -> ExtractionResult: ...


# ---------------------------------------------------------------------------
# LoCoMo loader
# ---------------------------------------------------------------------------

# The LoCoMo dataset lives in the original benchmarks/locomo/data/ location.
# Walk up from this file to the repo root, then descend into the data dir.
_THIS = Path(__file__).resolve().parent
for _candidate in (_THIS.parent, _THIS.parent.parent):
    if (_candidate / "benchmarks" / "locomo" / "data" / "locomo10.json").exists():
        DATA_PATH = _candidate / "benchmarks" / "locomo" / "data" / "locomo10.json"
        break
else:  # pragma: no cover
    DATA_PATH = _THIS.parent / "benchmarks" / "locomo" / "data" / "locomo10.json"

# LoCoMo QA categories (per the bench README, also locomo/loader.py).
QA_CATEGORIES = {
    1: "single-hop",
    2: "multi-hop",
    3: "temporal",
    4: "open-domain",
    5: "adversarial",
}


@dataclass
class QA:
    qid: str
    category: str
    question: str
    gold_answer: str
    evidence: List[str]  # list of dia_ids
    category_int: int


@dataclass
class Sample:
    sample_id: str
    conversation: List[Dict[str, Any]]  # raw session dicts (speaker_a, session_1, ...)
    speaker_a: Optional[str] = None
    speaker_b: Optional[str] = None
    qa: List[QA] = field(default_factory=list)
    event_summary: Any = None
    observation: Any = None
    session_summary: Any = None


def load_locomo(path: Optional[Path] = None) -> List[Sample]:
    """Load the official LoCoMo-10 dataset.

    The dataset is a list of 10 samples; each sample has a
    ``conversation`` object with ``session_1``..``session_N`` keys
    (each a list of turns with ``speaker``, ``text``, ``dia_id``)
    plus ``speaker_a`` / ``speaker_b`` and the QA list.
    """
    p = Path(path) if path else DATA_PATH
    if not p.exists():
        raise FileNotFoundError(f"LoCoMo dataset not found at {p}")
    raw = json.loads(p.read_text(encoding="utf-8"))
    if not isinstance(raw, list):
        raise ValueError(f"LoCoMo root must be a list, got {type(raw).__name__}")
    samples: List[Sample] = []
    for s in raw:
        if not isinstance(s, dict):
            continue
        samples.append(_parse_sample(s))
    return samples


def _parse_sample(d: Dict[str, Any]) -> Sample:
    conv = d.get("conversation") or {}
    if not isinstance(conv, dict):
        conv = {}
    qas = []
    for q in d.get("qa") or []:
        if not isinstance(q, dict):
            continue
        cat_int = int(q.get("category") or 0)
        cat_name = QA_CATEGORIES.get(cat_int, f"unknown({cat_int})")
        qas.append(
            QA(
                qid=str(q.get("question_id") or q.get("qid") or ""),
                category=cat_name,
                question=str(q.get("question") or ""),
                gold_answer=str(q.get("answer") or ""),
                evidence=list(q.get("evidence") or []),
                category_int=cat_int,
            )
        )
    return Sample(
        sample_id=str(d.get("sample_id") or ""),
        conversation=conv,
        speaker_a=conv.get("speaker_a"),
        speaker_b=conv.get("speaker_b"),
        qa=qas,
        event_summary=d.get("event_summary"),
        observation=d.get("observation"),
        session_summary=d.get("session_summary"),
    )


# ---------------------------------------------------------------------------
# Chunker
# ---------------------------------------------------------------------------

# The session date in LoCoMo looks like "1:56 pm on 8 May, 2023" — we
# extract it once per session and carry it through.
_SESSION_DATE_RE = re.compile(
    r"^(\d{1,2}:\d{2}\s*(?:am|pm)\s+on\s+\d{1,2}\s+\w+,?\s+\d{4})",
    re.IGNORECASE,
)


def _session_date_for(session_key: str, conv: Dict[str, Any]) -> str:
    """Look up the in-narrative session date for a session key.

    LoCoMo carries the date as ``session_1_date_time`` parallel to
    ``session_1``; fall back to the empty string when missing.
    """
    raw = conv.get(f"{session_key}_date_time")
    if isinstance(raw, list) and raw:
        first = raw[0]
        if isinstance(first, str):
            return first
    if isinstance(raw, str):
        return raw
    return ""


def chunks_from_turns(sample: Sample) -> List[Chunk]:
    """Flatten a LoCoMo sample's conversation into a list of Chunks.

    One Chunk = one conversation turn. ``session_date`` and
    ``speaker`` are the structured fields; ``dia_id`` is the turn's
    dialogue id (e.g. ``D1:3``), which is what the QA evidence uses.
    """
    out: List[Chunk] = []
    for session_key, turns in sample.conversation.items():
        if not isinstance(turns, list):
            continue
        if session_key.endswith("_date_time"):
            continue
        session_date = _session_date_for(session_key, sample.conversation)
        session_id = f"{sample.sample_id}::{session_key}"
        for t in turns:
            if not isinstance(t, dict):
                continue
            text = str(t.get("text") or "").strip()
            if not text:
                continue
            speaker = str(t.get("speaker") or "").strip()
            dia_id = str(t.get("dia_id") or "").strip()
            cid = f"{sample.sample_id}::{dia_id or session_key}"
            out.append(
                Chunk(
                    chunk_id=cid,
                    text=text,
                    session_id=session_id,
                    session_date=session_date,
                    speaker=speaker,
                    dia_id=dia_id,
                    metadata={
                        "sample_id": sample.sample_id,
                        "session_key": session_key,
                    },
                )
            )
    return out


def render_chunk(c: Chunk) -> str:
    """The text as the runner would ingest it: ``[date] Speaker: body``.

    Mirrors ``locomo.runner.render_turn``.
    """
    date = c.session_date or "<no-date>"
    speaker = c.speaker or "<no-speaker>"
    return f"[{date}] {speaker}: {c.text}"


# ---------------------------------------------------------------------------
# Misc helpers
# ---------------------------------------------------------------------------

def flatten_evidence(sample: Sample) -> List[str]:
    """All evidence dia_ids across the sample's QA."""
    out: List[str] = []
    for q in sample.qa:
        out.extend(q.evidence)
    return out


def category_breakdown(turns: List[Chunk]) -> Dict[str, int]:
    return {}


def timeit(fn, *args, **kwargs) -> Tuple[Any, float]:
    """Convenience: run ``fn(*args, **kwargs)`` and return ``(result, ms)``."""
    t0 = time.perf_counter()
    out = fn(*args, **kwargs)
    return out, (time.perf_counter() - t0) * 1000.0


def timed_extract(extractor, chunks: Sequence["Chunk"]) -> "ExtractionResult":
    """Run ``extractor.extract(chunks)`` and stamp the real wall-clock time.

    The individual extractors never populate ``stats.elapsed_ms`` (it would
    require every implementation to time itself), so the comparison's
    ``total ms`` column was always 0. This wrapper is the single, authoritative
    place timing is measured: the harness calls *this*, not ``extract``
    directly, so every extractor is timed identically and the number is real.
    """
    result, ms = timeit(extractor.extract, chunks)
    result.stats.elapsed_ms = ms
    return result
