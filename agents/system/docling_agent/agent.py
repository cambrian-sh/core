"""docling_agent (DeterministicAgent) — structure-aware document parsing sidecar.

The kernel hands it a document (text and/or raw bytes + source type) and it
returns the document's real hierarchy as a normalized `StructuredDocument`
(sections tree + leaf content, each leaf carrying its section breadcrumb + an
ltree-safe ordinal path). Phase 2 persists that tree as a structure graph.

Two backends, one contract:
  * Docling (high-fidelity, PDF/DOCX/PPTX/images) when installed + raw bytes are
    provided — real layout/table/figure hierarchy with reading-order + provenance.
  * A dependency-free Markdown/plain-text parser otherwise — always available,
    covers textbook-as-markdown and clean text.

Kept WARM like the other system agents: `serve()` blocks and any model load
(Docling's converter) is amortized across calls. Being a `DeterministicAgent` it
has no LLM by construction — parsing is deterministic.

Request  (JSON): {doc_id, title?, source_type?, text?, data_b64?}
Response (JSON): StructuredDocument.to_json() + {ok, error?}
"""
from __future__ import annotations

import base64
import json

from cambrian_agent_sdk import DeterministicAgent
from cambrian_agent_sdk._logging import configure_logging

from structure import parse_document
from docling_backend import DoclingUnavailable, docling_available, parse_with_docling

AGENT_DESCRIPTION = (
    "Deterministic structure-aware document parser: turns a document (text or bytes) "
    "into a normalized hierarchy (sections + leaf content with inherited section paths) "
    "via Docling (PDF/DOCX/images) or a Markdown/text parser. Kernel-invoked on ingest."
)

# Source types we prefer to route through Docling when raw bytes are available.
_BINARY_TYPES = {"pdf", "docx", "doc", "pptx", "ppt", "xlsx", "xls", "png", "jpg",
                 "jpeg", "tiff", "tif", "image", "binary"}


class DoclingAgent(DeterministicAgent):
    """Structure-aware parsing sidecar. `run` handles one document."""

    def __init__(self, agent_id: str = "docling_agent", **kwargs) -> None:
        super().__init__(agent_id=agent_id, description=AGENT_DESCRIPTION, **kwargs)
        self._docling = docling_available()

    def run(self, task):
        req = self._parse_request(task)
        doc_id = req.get("doc_id") or "document"
        title = req.get("title") or ""
        source_type = (req.get("source_type") or "").lower().lstrip(".")
        text = req.get("text") or ""
        data_b64 = req.get("data_b64") or ""

        try:
            structured, error = self._parse(doc_id, title, source_type, text, data_b64)
        except Exception as e:  # never crash the ingest path
            payload = {"ok": False, "error": f"{type(e).__name__}: {e}",
                       "doc_id": doc_id, "title": title, "source_type": source_type,
                       "backend": "none", "nodes": []}
            return json.dumps(payload).encode("utf-8")

        out = structured.to_json()
        out["ok"] = error == ""
        if error:
            out["error"] = error
        return json.dumps(out).encode("utf-8")

    # ── internals ────────────────────────────────────────────────────────────

    @staticmethod
    def _parse_request(task) -> dict:
        raw = task.data if getattr(task, "data", None) else (getattr(task, "text", "") or "").encode("utf-8")
        try:
            obj = json.loads(raw.decode("utf-8") if isinstance(raw, (bytes, bytearray)) else raw)
        except Exception:
            return {}
        return obj if isinstance(obj, dict) else {}

    def _parse(self, doc_id, title, source_type, text, data_b64):
        """Route to the right backend. Returns (StructuredDocument, error_str)."""
        want_docling = bool(data_b64) and (source_type in _BINARY_TYPES or not text)
        if want_docling:
            if self._docling:
                try:
                    data = base64.b64decode(data_b64)
                    return parse_with_docling(doc_id, data, source_type, title), ""
                except DoclingUnavailable as e:
                    # fall through to text if we have any, else report
                    if not text:
                        return parse_document(doc_id, "", source_type, title), f"docling unavailable: {e}"
                except Exception as e:
                    if not text:
                        return parse_document(doc_id, "", source_type, title), f"docling parse failed: {e}"
            elif not text:
                return (parse_document(doc_id, "", source_type, title),
                        "docling not installed and no text provided (pip install docling for PDF/DOCX)")
        # text/markdown path (always available)
        return parse_document(doc_id, text, source_type or "text", title), ""


agent = DoclingAgent(agent_id="docling_agent")


if __name__ == "__main__":
    configure_logging(agent_id="docling_agent")
    agent.serve()
