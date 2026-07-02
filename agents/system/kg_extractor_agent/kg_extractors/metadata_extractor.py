"""MetadataExtractor — deterministic, no LLM, no spaCy.

Parses the structured ``[date] Speaker: body`` prefix that the LoCoMo
runner adds at ingest time and emits the canonical relations every KG
construction pipeline would want:

* ``<speaker> --spoke to--> <other_speaker>``     (per turn, for every other participant)
* ``<this turn> --dated at--> <iso_date>``        (temporal grounding)
* ``<speaker> --spoke at--> <iso_date>``          (per-speaker timestamp)

The timestamp is normalised to ISO-8601 (``YYYY-MM-DDTHH:MM``) so the
KG has a *comparable* time, which is what the LoCoMo ``temporal`` category
needs.

This is the same pattern as ``benchmarks/locomo/extract_metadata_triplets.py``
(port from the script into a library class), generalised over a list of
chunks so the notebook can call it the same way as every other extractor.
"""
from __future__ import annotations

import re
from datetime import datetime
from typing import List, Optional, Sequence

from .common import (
    Chunk,
    ExtractionResult,
    ExtractionStats,
    Graph,
    META_KIND_NAMED,
    META_KIND_VALUED,
    Triplet,
)


_MONTHS = {
    "january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
    "july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
}

_TS_PAT = re.compile(
    r"\[?(\d{1,2}):(\d{2})\s*(am|pm)\s+on\s+(\d{1,2})\s+(\w+),?\s+(\d{4})\]?",
    re.IGNORECASE,
)


def _to_iso(h: str, m: str, ampm: str, day: str, month: str, year: str) -> Optional[str]:
    try:
        hh = int(h) % 12
        if ampm.lower() == "pm":
            hh += 12
        mon = _MONTHS.get(month.lower())
        if mon is None:
            return None
        return datetime(int(year), mon, int(day), hh, int(m)).strftime("%Y-%m-%dT%H:%M")
    except (ValueError, TypeError):
        return None


class MetadataExtractor:
    """Deterministic, 0-cost baseline. Always available; never raises."""

    name = "metadata"

    def extract(self, chunks: Sequence[Chunk]) -> ExtractionResult:
        # Group by session to discover the per-session participants
        per_session: dict = {}
        for c in chunks:
            per_session.setdefault(c.session_id, []).append(c)

        triplets: List[Triplet] = []
        for sid, chs in per_session.items():
            speakers = sorted({c.speaker for c in chs if c.speaker})
            for c in chs:
                # Parse the timestamp on the rendered text
                ts = None
                m = _TS_PAT.match(c.text or "") or _TS_PAT.match(c.session_date or "")
                if m:
                    ts = _to_iso(*m.groups())

                # Speaker --spoke to--> every other participant
                for other in speakers:
                    if other and other != c.speaker:
                        triplets.append(Triplet(
                            chunk_id=c.chunk_id,
                            subject=c.speaker or "<unknown>",
                            relation="spoke to",
                            object=other,
                            subject_kind=META_KIND_NAMED,
                            object_kind=META_KIND_NAMED,
                            source=self.name,
                        ))
                # Temporal grounding (only when the date parses)
                if ts:
                    triplets.append(Triplet(
                        chunk_id=c.chunk_id,
                        subject=c.dia_id or c.chunk_id,
                        relation="dated at",
                        object=ts,
                        subject_kind=META_KIND_NAMED,
                        object_kind=META_KIND_VALUED,
                        source=self.name,
                    ))
                    if c.speaker:
                        triplets.append(Triplet(
                            chunk_id=c.chunk_id,
                            subject=c.speaker,
                            relation="spoke at",
                            object=ts,
                            subject_kind=META_KIND_NAMED,
                            object_kind=META_KIND_VALUED,
                            source=self.name,
                        ))

        # Dedupe (the same triplet can be emitted by multiple chunks in the
        # same session — collapse identical (h, r, t) but keep all chunk_ids)
        seen = {}
        for t in triplets:
            key = (t.subject, t.relation, t.object)
            seen.setdefault(key, t)
        deduped = list(seen.values())

        # Build the in-memory graph from the DEDUPED triplets, so graph node
        # counts and the returned triplets describe the same population
        # (previously the graph was built from the raw, pre-dedup list).
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
