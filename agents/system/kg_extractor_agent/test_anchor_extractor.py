"""Unit tests for the deterministic AnchorExtractor tier.

Run standalone (no pytest dependency needed):
    PYTHONPATH=<kg_extractor_agent dir> python test_anchor_extractor.py
"""
from __future__ import annotations

from kg_extractors.anchor_extractor import AnchorExtractor, normalize_ordinal
from kg_extractors.common import Chunk


def _chunk(text: str, cid: str = "0", dia: str = "doc-1") -> Chunk:
    return Chunk(chunk_id=cid, text=text, session_id="s", session_date="",
                 speaker="", dia_id=dia, metadata={})


def _anchors(text: str, dia: str = "doc-1"):
    res = AnchorExtractor().extract([_chunk(text, dia=dia)])
    return {t.object for t in res.triplets}, res.triplets


FAILS = []


def check(name, cond):
    print(("PASS" if cond else "FAIL"), "-", name)
    if not cond:
        FAILS.append(name)


# --- normalize_ordinal ------------------------------------------------------
check("digit passthrough", normalize_ordinal("1") == "1")
check("padded digit", normalize_ordinal("007") == "7")
check("number word", normalize_ordinal("three") == "3")
check("roman numeral", normalize_ordinal("IV") == "4")
check("roman xii", normalize_ordinal("xii") == "12")
check("appendix letter", normalize_ordinal("B") == "b")
check("non-ordinal is None", normalize_ordinal("harbor") is None)

# --- tidebound paragraph (the real failure case) ----------------------------
TIDE = ("Chapter 1, scene 1: On Frostwane 4 at hour 6, Mara Vey, the cartographer, "
        "arrived at Blue Quay under fog. The coded phrase was COPPER-RAIN, and the "
        "ledger amount was 512 silver marks. This detail supersedes any earlier "
        "rumor that scene 1 used the market tunnel.")
vals, triplets = _anchors(TIDE, dia="tidebound-archive-chunk-1")
check("emits chapter:1", "chapter:1" in vals)
check("emits scene:1", "scene:1" in vals)
check("emits compound chapter_scene:1/1", "chapter_scene:1/1" in vals)
check("code word COPPER-RAIN is NOT an id anchor",
      not any(v.startswith("id:copper") for v in vals))
check("subject is the doc id (free id handle)",
      all(t.subject == "tidebound-archive-chunk-1" for t in triplets))
check("relation is has_anchor", all(t.relation == "has_anchor" for t in triplets))
check("chunk_id is positional", all(t.chunk_id == "0" for t in triplets))

# --- higher chapter/scene, roman + compound ---------------------------------
vals2, _ = _anchors("Chapter IV, scene 3: the rest of the paragraph body.")
check("roman chapter -> chapter:4", "chapter:4" in vals2)
check("compound chapter_scene:4/3", "chapter_scene:4/3" in vals2)

# --- decimal section --------------------------------------------------------
vals3, _ = _anchors("Section 3.2 covers the reconciliation of the ledger.")
check("decimal section:3.2", "section:3.2" in vals3)

# --- explicit ids -----------------------------------------------------------
vals4, _ = _anchors("Invoice INV-2024 references ticket #4217 under statute §12.")
check("explicit id INV-2024", "id:inv-2024" in vals4)
check("hash ticket #4217", "id:4217" in vals4)
check("statute §12", "statute:12" in vals4)

# --- no-anchor fallback -----------------------------------------------------
vals5, tr5 = _anchors("The harbor magistrate discussed the treaty with the diver.")
check("no reference system -> no anchors", vals5 == set() and tr5 == [])

# --- body cross-reference does not override the heading ---------------------
# The heading scopes the chunk; a later mention of a different scene number in
# the body must NOT change scene:1 (first match wins).
CROSS = "Chapter 2, scene 1: earlier notes mention scene 9 elsewhere in book 2."
vals6, _ = _anchors(CROSS)
check("first scene match wins (scene:1 not scene:9)",
      "scene:1" in vals6 and "scene:9" not in vals6)
check("compound uses heading values chapter_scene:2/1", "chapter_scene:2/1" in vals6)

print()
if FAILS:
    print(f"{len(FAILS)} FAILED: {FAILS}")
    raise SystemExit(1)
print("ALL PASSED")
