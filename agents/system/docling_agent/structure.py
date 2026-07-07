"""Normalized document-structure schema + a dependency-free structural parser.

This module defines the **contract** between the parsing sidecar (this Python
agent) and the Go graph-persistence layer (Phase 2): a `StructuredDocument` is a
flat list of `StructNode`s that together form a tree (via `parent_id`), where

  * `section` nodes are the hierarchy (chapter/section/subsection),
  * leaf nodes (`paragraph`/`table`/`figure`/`list`/`code`) are the retrievable
    content, each carrying the section it lives under,
  * every node carries an ordinal materialized `path` (ltree-safe, e.g.
    `n0.n2.n1`) and a human `section_path` breadcrumb (e.g.
    `Chapter 3 › Section 3.2`) — the "inherit the structural path onto every
    leaf" property that lets retrieval filter/expand by location.

The `parse_document` function is the always-available backend: it builds the
hierarchy from Markdown headings, or (for plain text) from conservative
heading-line detection. The Docling backend (docling_backend.py) produces the
same `StructuredDocument` from PDF/DOCX/etc. — same contract, richer source.
"""
from __future__ import annotations

import re
from dataclasses import asdict, dataclass, field
from typing import Any, Dict, List, Optional

# ── node kinds ──────────────────────────────────────────────────────────────
KIND_DOCUMENT = "document"
KIND_SECTION = "section"
KIND_PARAGRAPH = "paragraph"
KIND_LIST = "list"
KIND_TABLE = "table"
KIND_FIGURE = "figure"
KIND_CODE = "code"
KIND_CAPTION = "caption"

LEAF_KINDS = (KIND_PARAGRAPH, KIND_LIST, KIND_TABLE, KIND_FIGURE, KIND_CODE, KIND_CAPTION)


@dataclass
class StructNode:
    id: str                       # stable within the doc: "sec:n0.n2" / "chk:n0.n2.n1"
    kind: str                     # one of the KIND_* constants
    level: int                    # 0 = document root; 1..N heading depth; leaves inherit parent level
    title: str = ""               # heading text (sections); "" for leaves
    text: str = ""                # content (leaves); "" for pure sections
    parent_id: str = ""           # tree edge; "" only for the document root
    order: int = 0                # 0-based position among siblings (drives NEXT/PREV)
    path: str = ""                # ordinal materialized path, ltree-safe: "n0.n2.n1"
    section_path: str = ""        # human breadcrumb of ancestor SECTION titles
    page: Optional[int] = None    # provenance when the backend has it (Docling)
    meta: Dict[str, Any] = field(default_factory=dict)


@dataclass
class StructuredDocument:
    doc_id: str
    title: str
    source_type: str
    backend: str                  # "markdown" | "text" | "docling"
    nodes: List[StructNode] = field(default_factory=list)

    def to_json(self) -> Dict[str, Any]:
        return {
            "doc_id": self.doc_id,
            "title": self.title,
            "source_type": self.source_type,
            "backend": self.backend,
            "nodes": [asdict(n) for n in self.nodes],
        }

    # convenience accessors (used by tests / callers)
    def sections(self) -> List[StructNode]:
        return [n for n in self.nodes if n.kind == KIND_SECTION]

    def leaves(self) -> List[StructNode]:
        return [n for n in self.nodes if n.kind in LEAF_KINDS]


# Junk-leaf filter (OCR / layout artifacts). Two characterizable kinds:
#  * deterministic layout markers — Docling's fixed "image not available" template
#    for un-rasterized pictures;
#  * OCR garbage — near-empty / punctuation-only fragments ("!", "! ! !") with no
#    real words. The letter-ratio/word-count check is applied ONLY to prose
#    paragraphs, never to tables/code (a numeric table legitimately has few
#    letters). Keeps junk out of the chunk set entirely (it never becomes a node).
_JUNK_MARKERS = (
    "image not available",
    "pdfpipelineoptions",
    "generate_picture_images",
)
_WORD_RE = re.compile(r"[A-Za-z]{2,}")


def is_junk_leaf(kind: str, text: str) -> bool:
    t = (text or "").strip()
    if not t:
        return True
    low = t.lower()
    if any(m in low for m in _JUNK_MARKERS):
        return True
    if kind == KIND_PARAGRAPH:
        if len(_WORD_RE.findall(t)) < 2:
            return True
        if sum(ch.isalpha() for ch in t) / len(t) < 0.35:
            return True
    return False


class StructureBuilder:
    """Builds a StructuredDocument by streaming heading/leaf events. Maintains the
    open-section stack, assigns sibling order, ordinal paths, and breadcrumbs."""

    def __init__(self, doc_id: str, title: str, source_type: str, backend: str) -> None:
        self.doc = StructuredDocument(doc_id=doc_id, title=title, source_type=source_type, backend=backend)
        self.root = StructNode(id="doc", kind=KIND_DOCUMENT, level=0, title=title)
        self.doc.nodes.append(self.root)
        self.stack: List[StructNode] = [self.root]      # open sections, root first
        self._child_count: Dict[str, int] = {}          # parent_id -> next order

    def _order(self, parent_id: str) -> int:
        n = self._child_count.get(parent_id, 0)
        self._child_count[parent_id] = n + 1
        return n

    def _path(self, parent: StructNode, order: int) -> str:
        seg = f"n{order}"
        return f"{parent.path}.{seg}" if parent.path else seg

    def add_section(self, level: int, title: str) -> StructNode:
        # A heading of depth `level` closes any open section at the same-or-deeper depth.
        while len(self.stack) > 1 and self.stack[-1].level >= level:
            self.stack.pop()
        parent = self.stack[-1]
        order = self._order(parent.id)
        path = self._path(parent, order)
        crumb = f"{parent.section_path} › {title}" if parent.section_path else title
        node = StructNode(id=f"sec:{path}", kind=KIND_SECTION, level=level, title=title,
                          parent_id=parent.id, order=order, path=path, section_path=crumb)
        self.doc.nodes.append(node)
        self.stack.append(node)
        return node

    def add_leaf(self, kind: str, text: str, meta: Optional[Dict[str, Any]] = None,
                 page: Optional[int] = None) -> Optional[StructNode]:
        if is_junk_leaf(kind, text):
            return None  # OCR/layout junk — dropped before it can pollute retrieval
        parent = self.stack[-1]
        order = self._order(parent.id)
        path = self._path(parent, order)
        node = StructNode(id=f"chk:{path}", kind=kind, level=parent.level, text=text,
                          parent_id=parent.id, order=order, path=path,
                          section_path=parent.section_path, page=page, meta=meta or {})
        self.doc.nodes.append(node)
        return node


# ── heading detection ───────────────────────────────────────────────────────
_MD_HEADING = re.compile(r"^(#{1,6})\s+(.*\S)\s*$")

# Plain-text headings: a keyword + ordinal on its own short line, e.g.
# "Chapter 3", "Section 3.2 Photosynthesis", "Part II". Conservative on purpose
# (short line) so prose sentences mentioning "chapter 3" aren't treated as headings.
_KW = r"(?:chapter|section|subsection|part|book|unit|lecture|appendix|article|module)"
_TEXT_HEADING_KW = re.compile(
    r"^\s*(" + _KW + r")\s+([0-9]+|[ivxlcdm]+|[a-z])\b[)\.:\-\s]*(.*)$", re.IGNORECASE)
# Decimal-numbered heading: "3.2 Photosynthesis" (short, starts with a capital word).
_TEXT_HEADING_NUM = re.compile(r"^\s*(\d+(?:\.\d+){0,4})[)\.:\s]+([A-Z][^\n]{0,79})$")

_KW_LEVEL = {"part": 1, "book": 1, "chapter": 1, "unit": 1, "lecture": 1, "module": 1,
             "section": 2, "article": 2, "appendix": 2, "subsection": 3}

_MAX_TEXT_HEADING_LEN = 90


def _detect_text_heading(line: str):
    """Return (level, title) if `line` looks like a standalone heading, else None."""
    s = line.strip()
    if not s or len(s) > _MAX_TEXT_HEADING_LEN:
        return None
    m = _TEXT_HEADING_KW.match(s)
    if m:
        kw = m.group(1).lower()
        ordinal = m.group(2)
        rest = (m.group(3) or "").strip()
        level = _KW_LEVEL.get(kw, 2)
        title = f"{kw.capitalize()} {ordinal}" + (f": {rest}" if rest else "")
        return level, title
    m = _TEXT_HEADING_NUM.match(s)
    if m:
        number = m.group(1)
        level = min(6, number.count(".") + 1)
        return level, f"{number} {m.group(2).strip()}"
    return None


def parse_document(doc_id: str, text: str, source_type: str = "text", title: str = "") -> StructuredDocument:
    """Dependency-free structural parse of Markdown or plain text.

    Markdown `#` headings drive the hierarchy when present; otherwise conservative
    plain-text heading detection is used. Content between headings is emitted as
    paragraph leaves split on blank lines. Fenced code blocks are kept intact as
    `code` leaves. Always returns at least a document root.
    """
    lines = text.splitlines()
    is_md = any(_MD_HEADING.match(l) for l in lines[:400])
    b = StructureBuilder(doc_id, title or doc_id, source_type, "markdown" if is_md else "text")

    buf: List[str] = []
    in_fence = False

    def flush_paragraph() -> None:
        block = "\n".join(buf).strip()
        buf.clear()
        if block:
            b.add_leaf(KIND_PARAGRAPH, block)

    for raw in lines:
        line = raw.rstrip("\n")
        stripped = line.strip()

        # fenced code block: keep verbatim, never treat inner lines as headings
        if stripped.startswith("```"):
            if in_fence:
                buf.append(line)
                b.add_leaf(KIND_CODE, "\n".join(buf).strip())
                buf.clear()
                in_fence = False
            else:
                flush_paragraph()
                in_fence = True
                buf.append(line)
            continue
        if in_fence:
            buf.append(line)
            continue

        heading = None
        if is_md:
            m = _MD_HEADING.match(line)
            if m:
                heading = (len(m.group(1)), m.group(2).strip())
        else:
            heading = _detect_text_heading(line)

        if heading is not None:
            flush_paragraph()
            b.add_section(heading[0], heading[1])
        elif stripped == "":
            flush_paragraph()
        else:
            buf.append(line)

    if in_fence and buf:  # unterminated fence
        b.add_leaf(KIND_CODE, "\n".join(buf).strip())
        buf.clear()
    flush_paragraph()
    return b.doc
