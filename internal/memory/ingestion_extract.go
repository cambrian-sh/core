package memory

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// extraction is a document's chunk set as the pipeline will store it: the flat
// chunker's chunks, or — when the ADR-0060 structure parser is on and found leaf
// content — the parser's leaves. It is computed BEFORE anything of the document is
// written (ENG-2110), so a document whose content yields no usable text is refused
// with nothing behind it: no evidence row, no document row, no chunks, no derivation
// journal entry.
type extraction struct {
	chunks        []domain.Chunk
	structured    *StructuredDocument
	reps          []StructNode
	droppedLeaves int
	// why records what the parser said, for a refusal message: "" when there was
	// nothing to report.
	parseNote string
}

// extract chunks doc and, when structure parsing is on, parses it. Pure with respect
// to storage: it reads the parser sidecar and writes nothing (only the junk-drop
// counter moves).
func (im *IngestionManager) extract(ctx context.Context, doc domain.ExternalDocument, documentID string) extraction {
	clock := domain.StageClockFrom(ctx)
	t := time.Now()
	chunks, err := im.chunkerFor(doc).Chunk(ctx, &doc)
	if err != nil {
		slog.Warn("IngestionManager: chunker failed, falling back to OptionC", "source_uri", doc.SourceURI, "err", err)
		chunks, _ = OptionCChunker{}.Chunk(ctx, &doc)
	}
	clock.Since(StageChunk, t)
	return im.extractStructure(ctx, doc, documentID, chunks)
}

// extractStructure runs the ADR-0060 structure parser over doc (leaves-as-chunks):
// the parser's leaves ARE the chunk set, so chunk boundaries match the hierarchy
// exactly and every chunk's section stamp is correct by construction. Falls back to
// the flat chunks when parsing is off, fails, or yields no leaf content.
func (im *IngestionManager) extractStructure(ctx context.Context, doc domain.ExternalDocument, documentID string, chunks []domain.Chunk) extraction {
	ex := extraction{chunks: chunks}
	if im.structureParser == nil || im.structureStore == nil {
		if len(doc.Data) > 0 {
			ex.parseNote = "binary content needs the document parser, and none is configured"
		}
		return ex
	}
	clock := domain.StageClockFrom(ctx)
	// A binary document (doc.Data non-empty) travels to the sidecar as base64 so
	// the docling_agent runs its Docling backend on the ORIGINAL bytes. Text
	// documents keep the Text path (no sidecar decode, no backend). The agent
	// gates on this: want_docling = bool(data_b64) and (...), so leaving DataB64
	// unset makes the Docling backend unreachable regardless of source type.
	dataB64 := ""
	if len(doc.Data) > 0 {
		dataB64 = base64.StdEncoding.EncodeToString(doc.Data)
	}
	t := time.Now()
	parsed, perr := im.structureParser.Parse(ctx, StructureParseRequest{
		DocID: documentID, Title: doc.Title, SourceType: doc.SourceType,
		Text: doc.Body, DataB64: dataB64,
		// The sidecar classifies junk; the kernel decides (applyStructureJunkFilter).
		JunkMode: structureJunkModeMark,
	})
	clock.Since(StageStructureParse, t)
	t = time.Now()
	defer func() { clock.Since(StageStructureLeaves, t) }()
	if perr != nil {
		slog.WarnContext(ctx, "IngestionManager: structure parse failed; flat chunking", "doc", documentID, "err", perr)
		ex.parseNote = fmt.Sprintf("the document parser failed: %v", perr)
		return ex
	}
	if parsed == nil {
		slog.WarnContext(ctx, "IngestionManager: structure parser returned no document; flat chunking", "doc", documentID)
		ex.parseNote = "the document parser returned nothing"
		return ex
	}
	// The sidecar reports degraded parses (ok=false) and classifies junk
	// leaves. Both used to be consumed silently — a chronic sidecar failure
	// disabled the entire default ingest strategy with no line of evidence,
	// and junk drops were unrecoverable content loss nobody could audit.
	if !parsed.OK {
		slog.WarnContext(ctx, "IngestionManager: structure sidecar reported a degraded parse; using its partial output",
			"doc", documentID, "backend", parsed.Backend)
		ex.parseNote = fmt.Sprintf("the document parser could not read it (degraded parse, backend %q)", parsed.Backend)
	}
	// Drops only for backends in structure_junk_filter_backends (default:
	// docling); logs each drop with hash + sample and counts it.
	ex.droppedLeaves = im.applyStructureJunkFilter(ctx, documentID, parsed)
	if lc, reps := ChunksFromLeaves(parsed); len(lc) > 0 {
		ex.chunks, ex.structured, ex.reps = lc, parsed, reps
		return ex
	}
	slog.WarnContext(ctx, "IngestionManager: structure parse yielded no leaves; flat chunking",
		"doc", documentID, "backend", parsed.Backend, "dropped_leaves", parsed.DroppedLeaves)
	if ex.parseNote == "" {
		ex.parseNote = fmt.Sprintf("the document parser found no text in it (backend %q)", parsed.Backend)
	}
	return ex
}

// usable refuses an extraction with no usable text (ENG-2110): every chunk is empty,
// whitespace, or undecodable. The error wraps domain.ErrNoUsableText and names the
// document and the reason the caller can act on.
//
// A binary document counts only what the parser read from the FILE: its flat chunks
// are the Body, which on the binary lane holds at most the operator's context note,
// so a PNG with a note but no recognised text is still a file nothing was read from.
func (ex extraction) usable(doc domain.ExternalDocument) error {
	if len(doc.Data) == 0 || ex.structured != nil {
		for _, c := range ex.chunks {
			if domain.HasUsableText(c.Body) {
				return nil
			}
		}
	}
	// A parser note explains a FILE; a text body's reason is its own text.
	var why string
	switch {
	case len(doc.Data) > 0 && ex.parseNote != "":
		why = ex.parseNote
	case len(doc.Data) > 0:
		why = "no text was found in the file"
	case doc.Body == "":
		why = "the document is empty"
	default:
		why = "the text is whitespace only"
	}
	return fmt.Errorf("%w from %s: %s", domain.ErrNoUsableText, doc.IngestDisplayName(), why)
}
