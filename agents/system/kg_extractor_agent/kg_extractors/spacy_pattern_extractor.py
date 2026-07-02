"""SpaCyPatternExtractor — spaCy NER + custom dependency + Hearst patterns.

Builds on top of the SVO extractor: same NER + dep tree, plus
deterministic patterns that catch the cases plain SVO misses:

* Hearst 1992 ``is-a`` patterns ("Caroline is a great counselor")
* ``appos`` ("Melanie, a friend of mine, ...")
* ``acl`` / ``relcl`` ("the dog that Mel adopted")
* Possessive ``'s`` ("Mel's painting")
* Conjoined verb arguments ("she was born in 1879 and died in 1955")

These are the *hand-engineered* extensions that buy precision on
conversational text where vanilla SVO over-extracts. The combination is
the standard "high-precision NLP" pipeline the user's second article
(Adamchic 2025) calls out.
"""
from __future__ import annotations

from typing import List, Optional, Sequence

from .common import (
    Chunk,
    ExtractionResult,
    ExtractionStats,
    ExtractorUnavailable,
    Graph,
    META_KIND_CONCEPT,
    META_KIND_NAMED,
    META_KIND_VALUED,
    Triplet,
)
from .spacy_svo_extractor import SpaCySVOExtractor, _flat_span, _kind_for

try:
    import spacy  # type: ignore
    _SPACY_OK = True
except ImportError:  # pragma: no cover
    _SPACY_OK = False


class SpaCyPatternExtractor(SpaCySVOExtractor):
    """SpaCy + Hearst + appos + acl + possessive patterns."""

    name = "spacy_patterns"

    def __init__(self, model: str = "en_core_web_sm") -> None:
        super().__init__(model=model)
        # nlp already loaded by base __init__

    def _hearst(self, doc) -> List[Triplet]:
        """Hearst is-a: 'X is a Y', 'Y such as X', 'X, a Y, ...'."""
        out: List[Triplet] = []
        for tok in doc:
            # 1. "X is a/an Y" — copular with a det+NOUN predicate
            if tok.dep_ == "ROOT" and tok.pos_ == "AUX":
                subj = next((c for c in tok.children if c.dep_ == "nsubj"), None)
                attrs = [c for c in tok.children if c.dep_ in ("attr", "acomp", "oprd")]
                if subj and attrs:
                    for a in attrs:
                        out.append(Triplet(
                            chunk_id="<doc>",  # patched by caller
                            subject=_flat_span(subj),
                            relation="is a",
                            object=_flat_span(a),
                            subject_kind=META_KIND_NAMED,
                            object_kind=META_KIND_CONCEPT,
                            weight=0.85,
                            source=self.name,
                            extra={"pattern": "hearst_copula"},
                        ))
            # 2. appos — "Mel, a friend of mine, ..."
            if tok.dep_ == "appos":
                head = tok.head
                out.append(Triplet(
                    chunk_id="<doc>",
                    subject=_flat_span(head),
                    relation="is a",
                    object=_flat_span(tok),
                    subject_kind=META_KIND_NAMED,
                    object_kind=META_KIND_CONCEPT,
                    weight=0.8,
                    source=self.name,
                    extra={"pattern": "appos"},
                ))
        return out

    def _possessives(self, doc) -> List[Triplet]:
        out: List[Triplet] = []
        for tok in doc:
            if tok.pos_ == "NOUN" and any(c.pos_ == "PART" and c.dep_ == "case" for c in tok.children):
                # tok is the possessed noun; the possessor is the parent of the 's
                poss = tok.head
                if poss.dep_ == "poss":
                    out.append(Triplet(
                        chunk_id="<doc>",
                        subject=_flat_span(poss),
                        relation="has",
                        object=_flat_span(tok),
                        subject_kind=META_KIND_NAMED,
                        object_kind=META_KIND_CONCEPT,
                        weight=0.9,
                        source=self.name,
                        extra={"pattern": "possessive"},
                    ))
        return out

    def _acl_relcl(self, doc) -> List[Triplet]:
        """Relative clauses ('the dog that Mel adopted') and
        adjectival clauses ('painting made by Mel')."""
        out: List[Triplet] = []
        for tok in doc:
            if tok.dep_ in ("acl", "relcl"):
                head = tok.head
                # Find a verb in tok and walk its args
                verbs = [c for c in tok.children if c.pos_ == "VERB"] + (
                    [tok] if tok.pos_ == "VERB" else []
                )
                for v in verbs:
                    for c in v.children:
                        if c.dep_ in ("nsubj", "nsubjpass", "dobj", "attr", "oprd"):
                            out.append(Triplet(
                                chunk_id="<doc>",
                                subject=_flat_span(c),
                                relation=v.lemma_.lower(),
                                object=_flat_span(head),
                                subject_kind=META_KIND_NAMED,
                                object_kind=META_KIND_NAMED,
                                weight=0.75,
                                source=self.name,
                                extra={"pattern": tok.dep_},
                            ))
        return out

    def _extract_chunk(self, chunk: Chunk) -> List[Triplet]:
        # Parse the chunk ONCE, then run both the SVO baseline and the patterns
        # over the same doc (previously this parsed twice — once in super()'s
        # _extract_chunk and once here — doubling spaCy cost per chunk).
        doc = self._nlp(chunk.text)
        out = self._svo_from_doc(chunk, doc)
        for t in self._hearst(doc) + self._possessives(doc) + self._acl_relcl(doc):
            # patch chunk_id and fix the meta-kind
            out.append(Triplet(
                chunk_id=chunk.chunk_id,
                subject=t.subject,
                relation=t.relation,
                object=t.object,
                subject_kind=t.subject_kind,
                object_kind=t.object_kind,
                weight=t.weight,
                source=t.source,
                extra=t.extra,
            ))
        return out
