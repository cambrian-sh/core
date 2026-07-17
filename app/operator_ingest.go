package app

import (
	"path"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Binary source types the docling_agent will run its Docling backend on. This
// MUST stay a superset-free mirror of `_BINARY_TYPES` in
// agents/system/docling_agent/agent.py: the agent gates on
// `source_type in _BINARY_TYPES`, so a type we invent here that the agent does not
// know silently falls back to the text path — the document is still ingested, just
// flat. Keyed by lowercase extension WITHOUT the dot.
var doclingBinaryExts = map[string]string{
	"pdf":  "pdf",
	"docx": "docx",
	"doc":  "doc",
	"pptx": "pptx",
	"ppt":  "ppt",
	"xlsx": "xlsx",
	"xls":  "xls",
	"png":  "png",
	"jpg":  "jpg",
	"jpeg": "jpeg",
	"tiff": "tiff",
	"tif":  "tif",
}

// operatorIngestSourceType resolves an operator upload's SourceType from its
// filename extension. This is NOT agent-to-task routing — it is data-driven format
// dispatch (the chunker_registry pattern, ADR-0060), so the Zero-Hardcode Rule is
// not in play.
//
// It returns "operator_ingest" for anything it does not recognise, preserving the
// historical behaviour of the text lane exactly.
func operatorIngestSourceType(filename string) string {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(filename)), ".")
	if st, ok := doclingBinaryExts[ext]; ok {
		return st
	}
	return "operator_ingest"
}

// operatorIngestTitle prefers the uploaded filename — for a binary document the
// body is bytes, so deriving a title from text is impossible; and for a report
// "2026-W29-ops-review.pdf" is a better title than its first 80 characters anyway.
func operatorIngestTitle(req operator.IngestRequest) string {
	if req.Filename != "" {
		return req.Filename
	}
	title := []rune(req.Text)
	if len(title) > 80 {
		title = title[:80]
	}
	return string(title)
}

// applyIngestContext folds the operator's context note into the document body as a
// titled markdown section.
//
// WHY fold instead of storing it as metadata: metadata is not chunked and not
// embedded, so a context note kept beside the document could never be retrieved and
// so could never influence an answer — it would be decoration. As a body section it
// is chunked, embedded, and citable like any other content, and the ADR-0060 parser
// reads it as a real section (so it gets its own section_path).
//
// For a binary document the note cannot be folded into the bytes; it rides as
// Body instead, and the parser merges the text and Docling lanes.
func applyIngestContext(body, context string) string {
	context = strings.TrimSpace(context)
	if context == "" {
		return body
	}
	section := "## Context\n\n" + context
	if strings.TrimSpace(body) == "" {
		return section
	}
	return section + "\n\n" + body
}

// operatorIngestDoc builds the ExternalDocument for an operator upload (ADR-0047
// A2.4). It is the seam where a UI upload becomes a kernel ingest, so it is the one
// place that decides the format lane:
//
//   - binary (Content set): bytes travel in Data to the docling_agent's Docling
//     backend; SourceType is the concrete format ("pdf") so the agent's
//     _BINARY_TYPES gate opens.
//   - text (Text set): unchanged from the historical path — SourceType
//     "operator_ingest", body in Body.
//
// SourceURI carries the filename so chunker_registry's ext precedence
// (Resolve(SourceType, docExt(SourceURI))) can route it.
func operatorIngestDoc(req operator.IngestRequest) domain.ExternalDocument {
	sourceURI := req.Source
	if sourceURI == "" && req.Filename != "" {
		sourceURI = "operator_ingest://" + req.Filename
	}
	if sourceURI == "" {
		sourceURI = "operator_ingest://" + req.SessionID
	}
	// The registry routes on docExt(SourceURI); if the operator supplied a `source`
	// label with no extension, append the filename so the format is still routable.
	if req.Filename != "" && path.Ext(sourceURI) == "" {
		sourceURI = strings.TrimSuffix(sourceURI, "/") + "/" + req.Filename
	}

	doc := domain.ExternalDocument{
		SourceURI:  sourceURI,
		SourceType: operatorIngestSourceType(req.Filename),
		Title:      operatorIngestTitle(req),
		Body:       applyIngestContext(req.Text, req.Context),
		Author:     req.Author,
		Timestamp:  time.Now().UTC(),
		ThreadID:   req.SessionID,
		Tags:       req.Tags,
		Importance: req.Importance,
	}
	if len(req.Content) > 0 {
		doc.Data = req.Content
	}
	return doc
}
