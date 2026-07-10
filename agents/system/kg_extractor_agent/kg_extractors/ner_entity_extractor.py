"""NEREntityExtractor — entity-node extractor for kgExpand + coherence recall.

Motivation (2026-07-09): the retrieval graph (kgExpand associative expansion +
GraphCoherence ranking nudge) only ever reads the ENTITY ENDPOINTS of the
per-chunk triplets — never the relation label. The existing SVO extractor
(spacy_svo_extractor) emits ``relation = verb.lemma_`` with subject/object taken
from raw dependency tokens, which produces two problems the graph consumers care
about:

  1. JUNK / GENERIC nodes — pronouns ("she", "it") and bare common nouns
     ("forces", "country") become entities, creating FALSE bridges between
     unrelated chunks and wasting the expansion budget.
  2. FRAGMENTED nodes — "Angela Merkel" is stored as "merkel", "Air Defense
     Artillery" not at all; two chunks about the same entity fail to link, so
     the TRUE bridges kgExpand exists to surface are lost.

This extractor targets node QUALITY, not relations. It:

  * takes ENTITIES from spaCy NER spans (full "Angela Merkel", not a token),
    plus proper-noun noun-chunks NER misses;
  * drops pronouns / stopwords / ultra-generic single common nouns;
  * resolves pronouns to their nearest compatible antecedent via a lightweight
    rule-based coref (no model download); ``fastcoref`` can slot in later;
  * emits chunk-level co-occurrence edges (relation is a marker — the consumers
    ignore it) so every surviving entity is indexed as an endpoint, with a
    self-anchor for a lone entity so singletons are still registered.

No LLM. Deterministic. Same ``Extractor`` protocol as the SVO/pattern tiers, so
it drops into the kg_extractor_agent tier list unchanged.
"""
from __future__ import annotations

from typing import Any, Dict, List, Optional, Sequence, Tuple

from .common import (
    Chunk,
    ExtractionResult,
    ExtractionStats,
    ExtractorUnavailable,
    Graph,
    META_KIND_NAMED,
    META_KIND_VALUED,
    Triplet,
)

try:
    import spacy  # type: ignore
    _SPACY_OK = True
except ImportError:  # pragma: no cover
    _SPACY_OK = False

# NER labels we treat as graph-worthy named entities. Deliberately excludes the
# VALUED family (DATE/TIME/CARDINAL/...) from co-occurrence nodes — those crowd
# the graph without disambiguating chunks. (The metadata tier already handles
# temporal anchoring separately.)
_NAMED_NER = {
    "PERSON", "ORG", "GPE", "LOC", "FAC", "PRODUCT",
    "EVENT", "WORK_OF_ART", "LAW", "LANGUAGE", "NORP",
}

# Pronoun → the entity kind its antecedent must have, for the rule-based coref.
_PERSON_PRONOUNS = {"he", "him", "his", "she", "her", "hers", "himself", "herself"}
_GROUP_PRONOUNS = {"they", "them", "their", "theirs", "themselves"}
_THING_PRONOUNS = {"it", "its", "itself"}

# Generic single-token common nouns that slip through as "entities" and only add
# noise. Not exhaustive — the PROPN/NER gate does most of the work; this catches
# the frequent offenders seen in the live store.
_GENERIC_NOUNS = {
    "forces", "geography", "country", "city", "area", "region", "people",
    "government", "group", "member", "part", "place", "world", "state",
    "team", "company", "family", "war", "system", "war",
}

_MIN_ENTITY_CHARS = 2
_MAX_ENTITIES_PER_CHUNK = 12   # cap the pairwise fan-out
_MAX_PAIRS_PER_CHUNK = 40

_CO_OCCURS = "co_occurs"
_MENTIONS = "mentions"  # self-anchor relation for a lone entity


def _norm(s: str) -> str:
    return " ".join((s or "").strip().split())


class NEREntityExtractor:
    """spaCy-NER + rule-coref entity-node extractor.

    ``coref`` selects the coreference backend: ``"rule"`` (default, zero-dep) or
    ``"none"``. ``fastcoref`` is a planned third option once we judge it worth
    the model download.
    """

    name = "ner_coref"

    def __init__(self, model: str = "en_core_web_sm", coref: str = "rule") -> None:
        if not _SPACY_OK:
            raise ExtractorUnavailable("spaCy is not installed; pip install spacy")
        try:
            self._nlp_model = spacy.load(model)
        except Exception as e:  # noqa: BLE001 — spacy raises OSError on missing model
            raise ExtractorUnavailable(f"spaCy model {model!r} unavailable: {e}")
        self._coref = coref

    # ── entity collection ──────────────────────────────────────────────────

    def _named_entities(self, doc: Any) -> List[Tuple[str, str, int, int]]:
        """Return (text, kind, start_char, end_char) for graph-worthy NER spans
        plus proper-noun noun-chunks the NER tagger missed. Deduped by span.

        Two span extensions repair the frequent truncations (warts 1 & 2):
          * a NORP/GPE adjective is extended into its head noun so "Colombian" →
            "Colombian military";
          * a trailing number is appended so "National Cycle Route" → "…Route 57".
        """
        out: List[Tuple[str, str, int, int]] = []
        covered: List[Tuple[int, int]] = []  # extended (start_i, end_i) token ranges

        def _overlaps(s_i: int, e_i: int) -> bool:
            return any(not (e_i < cs or s_i > ce) for cs, ce in covered)

        for ent in getattr(doc, "ents", []):
            if ent.label_ not in _NAMED_NER:
                continue
            s_i, e_i = ent.start, ent.end - 1
            e_i = _extend_span_end(doc, ent[0].i, e_i, ent.label_)
            if _overlaps(s_i, e_i):
                continue
            span = doc[s_i:e_i + 1]
            txt = _strip_leading_det(_norm(span.text))
            if len(txt) < _MIN_ENTITY_CHARS:
                continue
            covered.append((s_i, e_i))
            out.append((txt, META_KIND_NAMED, span.start_char, span.end_char))

        # Proper-noun noun-chunks NER didn't already cover (e.g. "National Cycle
        # Route 57" that sm's NER may miss). Only chunks whose root is PROPN.
        for nc in getattr(doc, "noun_chunks", []):
            root = nc.root
            if root.pos_ != "PROPN":
                continue
            s_i, e_i = nc.start, nc.end - 1
            e_i = _extend_span_end(doc, root.i, e_i, "")
            if _overlaps(s_i, e_i):
                continue
            span = doc[s_i:e_i + 1]
            txt = _strip_leading_det(_norm(span.text))
            if len(txt) < _MIN_ENTITY_CHARS or txt.lower() in _GENERIC_NOUNS:
                continue
            covered.append((s_i, e_i))
            out.append((txt, META_KIND_NAMED, span.start_char, span.end_char))

        return out

    def _resolve_pronouns(
        self, doc: Any, ner: List[Tuple[str, str, int, int]]
    ) -> List[str]:
        """Rule-based coref: for each pronoun, attach the nearest PRECEDING
        compatible antecedent entity to the chunk's entity set. Returns the list
        of antecedent texts that pronouns resolved to (so a chunk that only
        refers to an entity pronominally still contributes that entity)."""
        if self._coref != "rule" or not ner:
            return []
        # antecedents sorted by position
        spans = sorted(ner, key=lambda x: x[2])
        resolved: List[str] = []
        for tok in doc:
            if tok.pos_ != "PRON":
                continue
            low = tok.text.lower()
            if low in _PERSON_PRONOUNS:
                want = "PERSON"
            elif low in _GROUP_PRONOUNS:
                want = "GROUP"   # PERSON or ORG/NORP
            elif low in _THING_PRONOUNS:
                want = "THING"   # ORG/GPE/LOC/PRODUCT/...
            else:
                continue
            ante = self._nearest_antecedent(doc, tok.idx, spans, want)
            if ante:
                resolved.append(ante)
        return resolved

    @staticmethod
    def _nearest_antecedent(
        doc: Any, pron_char: int, spans: List[Tuple[str, str, int, int]], want: str
    ) -> Optional[str]:
        """Closest entity span starting before the pronoun whose type fits."""
        best: Optional[str] = None
        best_pos = -1
        for txt, _kind, s_char, _e in spans:
            if s_char >= pron_char:
                break  # spans are position-sorted; nothing further can precede
            # We only have the coarse META kind here; refine using the doc's ents
            label = _label_at(doc, s_char)
            ok = (
                (want == "PERSON" and label == "PERSON")
                or (want == "GROUP" and label in ("PERSON", "ORG", "NORP"))
                or (want == "THING" and label in ("ORG", "GPE", "LOC", "FAC", "PRODUCT", "WORK_OF_ART", "EVENT"))
            )
            if ok and s_char > best_pos:
                best, best_pos = txt, s_char
        return best

    # ── triplet emission ───────────────────────────────────────────────────

    def _extract_chunk(self, chunk: Chunk) -> List[Triplet]:
        doc = self._nlp_model(chunk.text) if chunk.text else None
        if doc is None:
            return []
        ner = self._named_entities(doc)
        pron_ants = self._resolve_pronouns(doc, ner)

        # Build the chunk's entity SET: NER spans + coref antecedents, deduped
        # case-insensitively, capped. Preserve first-seen surface form.
        kind_by_norm: Dict[str, str] = {}
        order: List[str] = []
        for txt, kind, _s, _e in ner:
            k = txt.lower()
            if k in _GENERIC_NOUNS:
                continue
            if k not in kind_by_norm:
                kind_by_norm[k] = kind
                order.append(txt)
        for txt in pron_ants:
            k = txt.lower()
            if k not in kind_by_norm:
                kind_by_norm[k] = META_KIND_NAMED
                order.append(txt)

        entities = order[:_MAX_ENTITIES_PER_CHUNK]
        if not entities:
            return []

        out: List[Triplet] = []
        if len(entities) == 1:
            e = entities[0]
            out.append(Triplet(
                chunk_id=chunk.chunk_id, subject=e, relation=_MENTIONS, object=e,
                subject_kind=kind_by_norm[e.lower()], object_kind=kind_by_norm[e.lower()],
                weight=1.0, source=self.name, extra={"lone": True},
            ))
            return out

        # STAR topology, not pairwise: kgExpand + coherence only read the per-chunk
        # entity SET (both walk the H/T endpoints), never the h↔t pairing — so a hub
        # star registers every entity as an endpoint in O(n) rows instead of the
        # O(n²) full mesh. Hub = first-seen entity; the choice is immaterial to the
        # consumers (every entity still appears as an endpoint).
        hub = entities[0]
        hub_kind = kind_by_norm[hub.lower()]
        for other in entities[1:]:
            out.append(Triplet(
                chunk_id=chunk.chunk_id, subject=hub, relation=_CO_OCCURS, object=other,
                subject_kind=hub_kind, object_kind=kind_by_norm[other.lower()],
                weight=1.0, source=self.name,
            ))
        return out

    def extract(self, chunks: Sequence[Chunk]) -> ExtractionResult:
        triplets: List[Triplet] = []
        for c in chunks:
            try:
                triplets.extend(self._extract_chunk(c))
            except Exception:  # noqa: BLE001 — one bad chunk must not abort the batch
                continue
        seen: Dict[Tuple[str, str, str, str], Triplet] = {}
        for t in triplets:
            key = (t.chunk_id, t.subject.lower(), t.relation, t.object.lower())
            seen.setdefault(key, t)
        deduped = list(seen.values())
        graph = Graph()
        for t in deduped:
            graph.add_triplet(t)
        stats = ExtractionStats(
            extractor=self.name,
            num_chunks=len(chunks),
            num_triplets=len(deduped),
            num_entities=graph.node_count(),
            notes={"raw_triplets": len(triplets), "coref": self._coref},
        )
        return ExtractionResult(chunks=list(chunks), triplets=deduped, graph=graph, stats=stats)


def _extend_span_end(doc: Any, root_i: int, end_i: int, label: str) -> int:
    """Greedily push an entity span's END token forward to repair truncations.

    * NORP/GPE adjective → head noun: "Colombian" (+ "military") so the org name
      survives, not the demonym alone.
    * trailing number: "Route" (+ "57") so numbered routes/highways stay whole.

    Bounded to two appends; never crosses a sentence boundary.
    """
    n = len(doc)
    # NORP/GPE adjective directly modifying a following common noun.
    if label in ("NORP", "GPE") and end_i + 1 < n:
        nxt = doc[end_i + 1]
        if nxt.pos_ in ("NOUN", "PROPN") and not nxt.is_sent_start:
            end_i = nxt.i
    # Trailing number ("Route 57", "Highway 1", "Boeing 747").
    if end_i + 1 < n:
        nxt = doc[end_i + 1]
        if (nxt.pos_ == "NUM" or nxt.like_num) and not nxt.is_sent_start and not nxt.is_punct:
            end_i = nxt.i
    return end_i


def _strip_leading_det(txt: str) -> str:
    low = txt.lower()
    for det in ("the ", "a ", "an ", "this ", "that ", "these ", "those ", "its ", "their "):
        if low.startswith(det):
            return txt[len(det):]
    return txt


def _label_at(doc: Any, start_char: int) -> str:
    """The NER label of the entity beginning at start_char, or ''."""
    for ent in getattr(doc, "ents", []):
        if ent.start_char == start_char:
            return ent.label_
    return ""
