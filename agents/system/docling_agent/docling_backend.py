"""Docling-backed structural parser — high-fidelity PDF/DOCX/PPTX/image → the same
`StructuredDocument` schema the dependency-free parser produces.

Docling (IBM, open-source) converts a document into a `DoclingDocument` with a
real element hierarchy (title, section headers, paragraphs, tables, pictures,
lists, code, captions), reading-order correction, and page provenance, using
specialized layout (RT-DETR/DocLayNet) + table (TableFormer) models — not a
generative VLM. We walk its item tree and re-emit it through the SAME
StructureBuilder so downstream (graph persistence) sees one contract regardless
of source format.

The `docling` import is guarded: if it isn't installed, `parse_with_docling`
raises `DoclingUnavailable` and the agent falls back to the text/markdown parser.
Enable the rich path with:  python -m pip install docling
"""
from __future__ import annotations

import io
from typing import Optional

from structure import (
    KIND_CAPTION,
    KIND_CODE,
    KIND_FIGURE,
    KIND_LIST,
    KIND_PARAGRAPH,
    KIND_TABLE,
    StructuredDocument,
    StructureBuilder,
)


class DoclingUnavailable(RuntimeError):
    """docling is not installed (or failed to import)."""


def docling_available() -> bool:
    try:
        import docling  # noqa: F401
        return True
    except Exception:
        return False


# Map Docling item labels -> our leaf kinds. Section headers/titles are handled
# separately (they open hierarchy, they are not leaves).
_LEAF_LABEL_KIND = {
    "text": KIND_PARAGRAPH,
    "paragraph": KIND_PARAGRAPH,
    "list_item": KIND_LIST,
    "table": KIND_TABLE,
    "picture": KIND_FIGURE,
    "figure": KIND_FIGURE,
    "code": KIND_CODE,
    "caption": KIND_CAPTION,
    "formula": KIND_PARAGRAPH,
    "footnote": KIND_PARAGRAPH,
}
_SECTION_LABELS = {"title", "section_header", "subtitle_level_1"}


def _label(item) -> str:
    lab = getattr(item, "label", "") or ""
    return str(getattr(lab, "value", lab)).lower()


def _item_text(item, docling_doc) -> str:
    # Prefer a markdown export for tables; plain text otherwise.
    for attr in ("export_to_markdown", "export_to_text"):
        fn = getattr(item, attr, None)
        if callable(fn):
            try:
                out = fn(doc=docling_doc) if attr == "export_to_markdown" else fn()
                if isinstance(out, str) and out.strip():
                    return out.strip()
            except Exception:
                pass
    txt = getattr(item, "text", None)
    return txt.strip() if isinstance(txt, str) else ""


def _page_of(item) -> Optional[int]:
    prov = getattr(item, "prov", None)
    if prov:
        try:
            return int(getattr(prov[0], "page_no", None))
        except Exception:
            return None
    return None


def parse_with_docling(doc_id: str, data: bytes, source_type: str = "", title: str = "") -> StructuredDocument:
    """Convert a binary/rich document to a StructuredDocument via Docling.

    `data` is the raw document bytes. Raises DoclingUnavailable if docling is
    missing. Level is taken from Docling's own hierarchy depth as we iterate.
    """
    try:
        import os
        from docling.datamodel.base_models import DocumentStream, InputFormat
        from docling.datamodel.pipeline_options import PdfPipelineOptions
        from docling.document_converter import DocumentConverter, PdfFormatOption
    except Exception as e:  # pragma: no cover - exercised only without docling
        raise DoclingUnavailable(str(e))

    name = f"{doc_id}.{source_type}" if source_type else doc_id
    stream = DocumentStream(name=name, stream=io.BytesIO(data))
    # Born-digital PDFs carry a text layer, so skip OCR by default (much faster and
    # more accurate — layout + reading order still run). Set DOCLING_OCR=1 to force
    # OCR for scanned/image PDFs.
    pdf_opts = PdfPipelineOptions()
    pdf_opts.do_ocr = os.environ.get("DOCLING_OCR", "") == "1"
    converter = DocumentConverter(format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=pdf_opts)})
    result = converter.convert(stream)
    ddoc = result.document

    b = StructureBuilder(doc_id, title or (getattr(ddoc, "name", "") or doc_id), source_type or "binary", "docling")

    # iterate_items yields (item, level) in reading order with hierarchy depth.
    # We translate Docling depth into our section levels: a section header at
    # depth d opens a section at level d+1; leaves attach under the open section.
    for item, level in ddoc.iterate_items():
        lab = _label(item)
        if lab in _SECTION_LABELS:
            title_txt = _item_text(item, ddoc) or lab
            # Skip page-number / decorative "headers": a real heading has letters.
            # (Messy PDFs misclassify page numbers and ornaments as section headers.)
            if sum(ch.isalpha() for ch in title_txt) < 2:
                if title_txt.strip():
                    b.add_leaf(KIND_PARAGRAPH, title_txt)
                continue
            # Docling gives a 0-based depth; +1 so the top heading is level 1.
            b.add_section(max(1, int(level) + 1), title_txt)
        else:
            kind = _LEAF_LABEL_KIND.get(lab, KIND_PARAGRAPH)
            text = _item_text(item, ddoc)
            if text:
                b.add_leaf(kind, text, meta={"docling_label": lab}, page=_page_of(item))
    return b.doc
