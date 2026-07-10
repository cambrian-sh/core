"""Side-by-side demo/judge: old SVO extractor vs new NER+coref entity extractor.

Runs both over the SAME sample chunks — the exact described-anchor cases that
went gold-absent in the MuSiQue misses — and prints the entities + triplets each
produces, so we can eyeball whether the NER+coref extractor gives clean, full,
coref-resolved entity NODES (what kgExpand + coherence need) vs the SVO tier's
pronoun/fragment/copula output.

Plain runnable script (PASS/FAIL prints), not pytest — matches the agent test
convention. Run from the kg_extractor_agent dir:  python test_ner_entity_extractor.py
"""
from __future__ import annotations

from kg_extractors.common import Chunk
from kg_extractors.spacy_svo_extractor import SpaCySVOExtractor
from kg_extractors.spacy_pattern_extractor import SpaCyPatternExtractor
from kg_extractors.ner_entity_extractor import NEREntityExtractor

SAMPLES = [
    # (id, text) — the described-anchor miss cases + one conversational chunk.
    ("merkel", "Angela Merkel was born in Hamburg. She served as Chancellor of "
               "Germany. Her home country was formerly East Germany, which had "
               "border troops called the Deutsche Grenzpolizei."),
    ("ada", "The Air Defense Artillery is a branch of the Colombian military. "
            "It was unprepared for the invasion of the territory the Nazis occupied."),
    ("emg", "European Movement Germany is a member of the European Movement. "
            "The European Movement promotes European integration across the continent."),
    ("ncr", "National Cycle Route 57 is part of the National Cycle Network. "
            "The network is an example of a sustainable transport initiative in Britain."),
    ("convo", "[2:24 pm on 14 August, 2023] Caroline: I met Devon at the Aurora "
              "conference and he introduced me to Maya from DeepMind."),
]


def _chunks():
    return [Chunk(chunk_id=cid, text=t, session_id="demo", session_date="",
                  speaker="", dia_id=cid, metadata={}) for cid, t in SAMPLES]


def _fmt(t):
    return f"({t.subject} [{t.subject_kind}]) --{t.relation}--> ({t.object} [{t.object_kind}])"


def main() -> int:
    chunks = _chunks()

    print("=" * 78)
    print("NEW: NEREntityExtractor (spaCy NER + rule coref)  — entity NODES for the graph")
    print("=" * 78)
    ner = NEREntityExtractor(coref="rule")
    res_new = ner.extract(chunks)
    by_chunk_new = {}
    for t in res_new.triplets:
        by_chunk_new.setdefault(t.chunk_id, []).append(t)
    for cid, text in SAMPLES:
        print(f"\n[{cid}] {text[:70]}...")
        ents = sorted({t.subject for t in by_chunk_new.get(cid, [])} |
                      {t.object for t in by_chunk_new.get(cid, [])})
        print(f"  entities: {ents}")
        for t in by_chunk_new.get(cid, []):
            print(f"    {_fmt(t)}")
    print(f"\n  stats: {res_new.stats.num_triplets} triplets, "
          f"{res_new.stats.num_entities} distinct nodes")

    print("\n" + "=" * 78)
    print("OLD: SpaCyPatternExtractor (SVO + Hearst/appos/etc.)  — verb-relation triplets")
    print("=" * 78)
    old = SpaCyPatternExtractor()
    res_old = old.extract(chunks)
    by_chunk_old = {}
    for t in res_old.triplets:
        by_chunk_old.setdefault(t.chunk_id, []).append(t)
    for cid, text in SAMPLES:
        print(f"\n[{cid}] {text[:70]}...")
        ents = sorted({t.subject for t in by_chunk_old.get(cid, [])} |
                      {t.object for t in by_chunk_old.get(cid, [])})
        print(f"  entities: {ents}")
        for t in by_chunk_old.get(cid, []):
            print(f"    {_fmt(t)}")
    print(f"\n  stats: {res_old.stats.num_triplets} triplets, "
          f"{res_old.stats.num_entities} distinct nodes")

    # crude judge signals
    print("\n" + "-" * 78)
    new_nodes = {n.lower() for n in res_new.graph.nodes}
    old_nodes = {n.lower() for n in res_old.graph.nodes}
    for target in ("angela merkel", "air defense artillery", "european movement germany",
                   "national cycle route 57"):
        print(f"  full-entity present?  {target!r:32}  NEW={target in new_nodes}  OLD={target in old_nodes}")
    junk = {"she", "it", "her", "he", "which", "network"}
    print(f"  NEW junk nodes: {sorted(new_nodes & junk)}")
    print(f"  OLD junk nodes: {sorted(old_nodes & junk)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
