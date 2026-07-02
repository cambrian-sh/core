"""SpaCySVOExtractor — Subject-Verb-Object baseline built on spaCy NER + dep parse.

This is the kg_extractor tier-2 (spacy_patterns) base class. The patterns
extension (SpaCyPatternExtractor, this package) inherits and adds Hearst
1992 is-a patterns, appos, acl/relcl, and possessive 's — together they
form the deterministic KG front-end documented in ADR-0053 D2 (revised).

The SVO walk is the standard dependency-parse extraction:
  for each ROOT verb:
    subject = the nsubj / nsubjpass child
    object  = the dobj / attr / oprd child
    emit (subject, verb.lemma_, object) with meta-kinds derived from POS+NER.

The walk is intentionally simple — a hand-engineered baseline that covers
the dominant conversational pattern ("X did Y to Z") at high precision.
Pattern fan-out is the layer above (SpaCyPatternExtractor).
"""
from __future__ import annotations

from typing import Any, List, Sequence

from .common import (
    Chunk,
    ExtractionResult,
    ExtractionStats,
    ExtractorUnavailable,
    Graph,
    META_KIND_CONCEPT,
    META_KIND_LOCATED,
    META_KIND_NAMED,
    META_KIND_VALUED,
    Triplet,
)

try:
    import spacy  # type: ignore
    from spacy.tokens import Doc, Token  # type: ignore
    _SPACY_OK = True
except ImportError:  # pragma: no cover
    _SPACY_OK = False
    Doc = Any  # type: ignore
    Token = Any  # type: ignore


# Meta-kind resolution from a spaCy token. Order matters: a PROPN with a
# fine-grained NER label is NAMED, a cardinal/number is VALUED, etc.
_NAMED_NER = {"PERSON", "ORG", "GPE", "LOC", "FAC", "PRODUCT", "EVENT", "WORK_OF_ART", "LANGUAGE", "LAW", "NORP"}
_LOCATED_NER = set()  # spaCy's en_core_web_sm doesn't tag filesystem paths; kept for future


def _kind_for(tok: Any) -> str:
    """Map a spaCy token to a Cambrian meta-kind (named / located / valued / concept)."""
    ent = getattr(tok, "ent_type_", "") or ""
    if ent in _NAMED_NER:
        return META_KIND_NAMED
    if ent in _LOCATED_NER:
        return META_KIND_LOCATED
    if getattr(tok, "pos_", "") in ("NUM",) or ent in ("DATE", "TIME", "MONEY", "PERCENT", "QUANTITY", "ORDINAL", "CARDINAL"):
        return META_KIND_VALUED
    if getattr(tok, "pos_", "") == "PROPN":
        return META_KIND_NAMED
    return META_KIND_CONCEPT


def _flat_span(tok: Any) -> str:
    """The flat text of a token's subtree. Trims surrounding whitespace."""
    if tok is None:
        return ""
    txt = getattr(tok, "text", "") or ""
    if not txt:
        subtree = getattr(tok, "subtree", None)
        if subtree is not None:
            txt = " ".join(t.text for t in subtree if t is not None)
    return " ".join((txt or "").split())


# Verb roles we accept as the relation-bearing node in an SVO triple.
_VERB_DEP_ROOTS = {"ROOT"}
# Subject roles.
_SUBJ_DEPS = {"nsubj", "nsubjpass", "expl"}
# Object roles — direct object, clausal complement, attribute, open clausal complement.
_OBJ_DEPS = {"dobj", "attr", "oprd", "acomp", "xcomp", "ccomp"}


class SpaCySVOExtractor:
    """SpaCy-backed Subject-Verb-Object baseline.

    The class is intentionally light: a single dep-parse walk per chunk,
    emitting one Triplet per (subject, verb, object) triple. The patterns
    layer above (SpaCyPatternExtractor) builds on this with Hearst/appos/
    possessive/acl/relcl rules.
    """

    name = "svo"

    def __init__(self, model: str = "en_core_web_sm") -> None:
        if not _SPACY_OK:
            raise ExtractorUnavailable("spaCy is not installed; pip install spacy")
        try:
            self._nlp_model = spacy.load(model)
        except Exception as e:  # noqa: BLE001 — spacy raises OSError on missing model
            raise ExtractorUnavailable(f"spaCy model {model!r} unavailable: {e}")

    def _nlp(self, text: str) -> Any:
        """Run the loaded pipeline on text. Thin alias for the SVO walk."""
        if not text:
            return None
        return self._nlp_model(text)

    def _svo_from_doc(self, chunk: Chunk, doc: Any) -> List[Triplet]:
        """Walk the dep tree and emit one (h, r, t) per (subj, verb, obj) triple."""
        if doc is None:
            return []
        out: List[Triplet] = []
        for tok in doc:
            if tok.dep_ not in _VERB_DEP_ROOTS or tok.pos_ not in ("VERB", "AUX"):
                continue
            verb = tok
            subj = next((c for c in verb.children if c.dep_ in _SUBJ_DEPS), None)
            if subj is None:
                # try the head's subject (copular constructions)
                head_subj = next(
                    (c for c in doc if c.dep_ in _SUBJ_DEPS and c.head == verb),
                    None,
                )
                subj = head_subj
            obj = next(
                (c for c in verb.children if c.dep_ in _OBJ_DEPS),
                None,
            )
            if subj is None or obj is None:
                continue
            sub_text = _flat_span(subj)
            obj_text = _flat_span(obj)
            rel = (verb.lemma_ or verb.text or "").lower().strip()
            if not sub_text or not obj_text or not rel:
                continue
            out.append(Triplet(
                chunk_id=chunk.chunk_id,
                subject=sub_text,
                relation=rel,
                object=obj_text,
                subject_kind=_kind_for(subj),
                object_kind=_kind_for(obj),
                weight=0.7,
                source=self.name,
                extra={"verb_token": verb.text, "subj_dep": subj.dep_, "obj_dep": obj.dep_},
            ))
        return out

    def _extract_chunk(self, chunk: Chunk) -> List[Triplet]:
        """Parse the chunk once and run the SVO walk over the resulting doc."""
        doc = self._nlp(chunk.text)
        return self._svo_from_doc(chunk, doc)

    def extract(self, chunks: Sequence[Chunk]) -> ExtractionResult:
        """Run SVO over every chunk and aggregate into an ExtractionResult.

        Triplets are deduped on (subject, relation, object) — the same triple
        appearing in multiple turns collapses to one row, with the first chunk
        carrying the chunk_id (the rest are dropped from the chunk_id view but
        their existence is what matters for evaluation).
        """
        triplets: List[Triplet] = []
        for c in chunks:
            try:
                triplets.extend(self._extract_chunk(c))
            except Exception:  # noqa: BLE001 — one bad chunk must not abort the batch
                continue
        seen = {}
        for t in triplets:
            key = (t.subject, t.relation, t.object)
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
            notes={"raw_triplets": len(triplets), "deduped": len(deduped)},
        )
        return ExtractionResult(chunks=list(chunks), triplets=deduped, graph=graph, stats=stats)
