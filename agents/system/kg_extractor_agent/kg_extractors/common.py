"""Common data structures and the Extractor protocol.

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

import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Protocol, Sequence, Tuple

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
# open "extracted" verb phrases from ADR-0053 chunk_triplets. Mirrors
# domain.EdgeType in cambrian-core/domain/graph.go. "discussed_in", "engaged"
# and "follows" were removed 2026-11-05 with scenes and the raw step-record
# path (cambrian-knowledge/decisions/scenes-cancelled.md).
TYPED_PREDICATES = (
    "closes",
    "specifies",
    "contradicts",
    "co_activated",
    "extracted",
)


@dataclass(frozen=True)
class Chunk:
    """One unit of text + provenance.

    Any text source fits this shape (a document chunk, a conversation turn, a
    tool output).
    """

    chunk_id: str
    text: str
    session_id: str
    session_date: str
    speaker: str
    dia_id: str
    metadata: Dict[str, Any] = field(default_factory=dict)


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
# Misc helpers
# ---------------------------------------------------------------------------

# The LoCoMo dataset loader and the LoCoMo turn chunking that used to sit here
# moved to the benchmark harness on 2026-11-14 (ENG-1448,
# decisions/no-benchmark-in-oss.md): cambrian-benchmarks
# src/cambrian_bench/suites/locomo/kg_turn_chunks.py. This OSS system agent keeps
# only the extractor types and helpers its production path uses.

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
