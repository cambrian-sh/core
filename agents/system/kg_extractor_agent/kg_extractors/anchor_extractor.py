"""AnchorExtractor — deterministic document-local reference extraction (no LLM, no spaCy).

Documents carry *local reference systems* independent of domain: section
headings (``Chapter 1``, ``Appendix B``, ``3.2``), ordinal member labels
(``scene 4``, ``Step 7``, ``Item 12``, ``Clause 3``), and explicit IDs
(``INV-2024``, ``#4217``, ``tidebound-archive-chunk-5``). These are the
query-time handles that pure dense retrieval cannot discriminate when every
chunk shares the same prose template — the exact failure mode the
document-QA benchmark stresses.

This tier emits ``<doc_id --has_anchor--> kind:value>`` triplets so the kernel's
existing chunk-triplet store (ADR-0053) can resolve them the same way it
resolves entities: ``ChunksMentioningEntity("chapter_scene:1/1")`` returns the
chunk that carries that anchor. It is a *sibling* of MetadataExtractor, not a
replacement — entities keep flowing from the spaCy/LLM tiers; anchors are the
new deterministic, high-precision structural tier.

Normalization (ingest and query MUST agree — the Go query side mirrors this):
  * ``kind:value``, lowercase — ``chapter:1``, ``scene:4``, ``section:3.2``, ``step:7``
  * roman / number-word ordinals folded to ints — ``Chapter IV`` -> ``chapter:4``
  * a COMPOUND anchor for a nested (container, member) pair, because the pair is
    the unique handle — ``Chapter 1, scene 1`` -> ``chapter_scene:1/1``
    (``chapter:1`` alone spans ten scenes; only the pair identifies one chunk)
  * the chunk's own document id is the triplet SUBJECT, so a query citing a
    chunk id resolves by exact match with no extra edge.

Never raises (mirrors MetadataExtractor): a chunk with no reference system
simply yields no anchor triplets, and retrieval falls back to hybrid search.
"""
from __future__ import annotations

import re
from typing import Dict, List, Optional, Sequence, Tuple

from .common import (
    Chunk,
    ExtractionResult,
    ExtractionStats,
    Graph,
    META_KIND_NAMED,
    META_KIND_VALUED,
    Triplet,
)

RELATION = "has_anchor"

# Container-tier reference kinds (the "chapter"-like level) and member-tier
# kinds (the "scene"-like level). A chunk that carries one of each gets a
# COMPOUND anchor container_member:c/m — the only handle that is unique when a
# member number repeats across containers.
_CONTAINER_KINDS = ("chapter", "part", "book", "act", "section", "article", "title")
_MEMBER_KINDS = ("scene", "step", "item", "clause", "verse", "stanza", "paragraph", "page", "line")
_OTHER_KINDS = ("appendix", "figure", "table")

# Ordinal token: arabic digits, roman numerals, or a single alphabetic token
# (number-word like "three", or a bare letter like Appendix "B").
_ORD = r"(\d{1,4}|[A-Za-z]+)"

# One compiled pattern per reference kind: "<kind> <ordinal>" with a word
# boundary. Case-insensitive. We take the FIRST match per kind (the heading),
# which is the reference that scopes the whole chunk; later body mentions of a
# different number don't override it.
_KIND_PATTERNS: Dict[str, "re.Pattern"] = {
    kind: re.compile(r"\b" + kind + r"\s+" + _ORD + r"\b", re.IGNORECASE)
    for kind in (*_CONTAINER_KINDS, *_MEMBER_KINDS, *_OTHER_KINDS)
}

# Decimal section reference: 3.2, 4.10.1 — a self-contained hierarchical locator.
_DECIMAL_SECTION = re.compile(r"\b(\d+(?:\.\d+)+)\b")

# Explicit IDs: SKU/ticket/invoice-style CODE-1234, statute §12, hash #4217.
# Deliberately requires a trailing DIGIT run so pure-letter code words
# (COPPER-RAIN) are NOT captured as ids — those are content, not anchors.
_EXPLICIT_ID = re.compile(r"\b([A-Za-z]{2,}-\d[\w-]*)\b")
_STATUTE_ID = re.compile(r"§\s*(\d+(?:\.\d+)*[a-z]?)")
_HASH_ID = re.compile(r"#(\d{2,})\b")

_ROMAN = {"i": 1, "v": 5, "x": 10, "l": 50, "c": 100, "d": 500, "m": 1000}
_WORDS = {
    "zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
    "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
    "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
    "eighteen": 18, "nineteen": 19, "twenty": 20, "first": 1, "second": 2,
    "third": 3, "fourth": 4, "fifth": 5, "sixth": 6, "seventh": 7, "eighth": 8,
    "ninth": 9, "tenth": 10,
}


def _roman_to_int(s: str) -> Optional[int]:
    total, prev = 0, 0
    for ch in reversed(s.lower()):
        v = _ROMAN.get(ch)
        if v is None:
            return None
        if v < prev:
            total -= v
        else:
            total += v
            prev = v
    return total if total > 0 else None


def normalize_ordinal(token: str) -> Optional[str]:
    """Fold an ordinal token to its canonical string form.

    Digits pass through ('1' -> '1'); number-words and roman numerals fold to
    ints ('three' -> '3', 'iv' -> '4'); a lone letter (Appendix B) stays a
    lowercase letter. Returns None for tokens that name no ordinal.
    """
    t = token.strip().lower()
    if not t:
        return None
    if t.isdigit():
        return str(int(t))
    if t in _WORDS:
        return str(_WORDS[t])
    # Roman numeral (only if it's a valid all-roman token). Checked before the
    # single-letter case so 'i'/'v'/'x' fold to ints, not letters.
    if all(c in _ROMAN for c in t):
        r = _roman_to_int(t)
        if r is not None:
            return str(r)
    # A single alphabetic label (Appendix B) — keep as-is, it IS the locator.
    if len(t) == 1 and t.isalpha():
        return t
    return None


class AnchorExtractor:
    """Deterministic, 0-cost structural-anchor tier. Always available; never raises."""

    name = "anchor"

    def extract(self, chunks: Sequence[Chunk]) -> ExtractionResult:
        triplets: List[Triplet] = []
        for c in chunks:
            triplets.extend(self._extract_chunk(c))

        # Dedupe identical (subject, relation, object) — a value can be matched
        # by more than one pattern in the same chunk.
        seen: Dict[Tuple[str, str, str], Triplet] = {}
        for t in triplets:
            seen.setdefault((t.subject, t.relation, t.object), t)
        deduped = list(seen.values())

        graph = Graph()
        for t in deduped:
            graph.add_triplet(t)

        stats = ExtractionStats(
            extractor=self.name,
            num_chunks=len(chunks),
            num_triplets=len(deduped),
            num_entities=graph.node_count(),
            notes={"raw_triplets": len(triplets), "deduped": len(deduped)},
        )
        return ExtractionResult(chunks=list(chunks), triplets=deduped, graph=graph, stats=stats)

    # -- internals -------------------------------------------------------------

    def _extract_chunk(self, chunk: Chunk) -> List[Triplet]:
        text = chunk.text or ""
        if not text.strip():
            return []
        # Subject is the chunk's real document id (dia_id), so a query that cites
        # the chunk id resolves by exact match; fall back to the positional id.
        subject = (chunk.dia_id or chunk.chunk_id or "").strip()
        if not subject:
            return []

        anchors = self._anchors_for(text)
        if not anchors:
            return []

        out: List[Triplet] = []
        for value in anchors:
            out.append(Triplet(
                chunk_id=chunk.chunk_id,
                subject=subject,
                relation=RELATION,
                object=value,
                subject_kind=META_KIND_NAMED,
                object_kind=META_KIND_VALUED,
                weight=1.0,
                source=self.name,
                extra={"anchor": value},
            ))
        return out

    def _anchors_for(self, text: str) -> List[str]:
        """Return the normalized anchor values for one chunk's text, in a stable
        order: atomic kind:value first, then the compound container_member, then
        decimal sections and explicit ids."""
        kind_values: Dict[str, str] = {}  # kind -> first normalized value seen
        for kind, pat in _KIND_PATTERNS.items():
            m = pat.search(text)
            if not m:
                continue
            norm = normalize_ordinal(m.group(1))
            if norm is not None:
                kind_values[kind] = norm

        values: List[str] = [f"{kind}:{val}" for kind, val in kind_values.items()]

        # Compound: pair the first present container with the first present
        # member (e.g. chapter:1 + scene:1 -> chapter_scene:1/1). This is the
        # unique handle the atomic anchors can't provide on their own.
        container = next((k for k in _CONTAINER_KINDS if k in kind_values), None)
        member = next((k for k in _MEMBER_KINDS if k in kind_values), None)
        if container and member:
            values.append(f"{container}_{member}:{kind_values[container]}/{kind_values[member]}")

        # Decimal sections (3.2) — self-contained locators.
        for m in _DECIMAL_SECTION.finditer(text):
            values.append(f"section:{m.group(1)}")

        # Explicit ids.
        for pat, prefix in ((_EXPLICIT_ID, "id"), (_STATUTE_ID, "statute"), (_HASH_ID, "id")):
            for m in pat.finditer(text):
                values.append(f"{prefix}:{m.group(1).lower()}")

        # Preserve first-seen order, dedupe.
        seen = set()
        ordered: List[str] = []
        for v in values:
            if v not in seen:
                seen.add(v)
                ordered.append(v)
        return ordered
