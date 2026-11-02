package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"
)

// Structure junk filter (ADR-0060; T-20261102-ingestion-structure-junk-filter-silently-drops).
//
// The docling_agent CLASSIFIES leaves that look like OCR/layout artifacts (a
// Docling layout marker such as "image not available", or a prose paragraph with
// almost no letters) and, under junk mode "mark", keeps them all, tagging each with
// meta.junk_reason. The DECISION to drop is made here, per parser backend, from
// execution.ingestion.structure_junk_filter_backends.
//
// It used to be made in the sidecar, unconditionally, for every backend. The
// text/markdown parser emits every non-fenced block as a "paragraph", so on chat
// sessions the letter-ratio rule deleted markdown tables, terminal output, number
// and colour lists, and an assistant turn listing the user's own receipts — and
// the kernel saw only a count. A memory product that silently loses what the user
// said is broken, so: the default drops nothing outside Docling output, and every
// drop that remains is logged with enough to audit it (kind, reason, length, a
// content hash and a sample) and counted.

// structureJunkModeMark asks the sidecar to keep and tag junk leaves rather than
// drop them. Absent, the sidecar keeps its legacy drop behaviour (so an older
// kernel binary is unaffected by a newer sidecar).
const structureJunkModeMark = "mark"

// structMetaJunkReason is the leaf meta key the sidecar sets on a classified leaf.
const structMetaJunkReason = "junk_reason"

// junkSampleRunes bounds the content sample logged for a dropped leaf.
const junkSampleRunes = 120

// junkSummaryLeaves bounds how many dropped leaves the per-document WARN line
// describes; the DEBUG line per leaf has the rest.
const junkSummaryLeaves = 5

// droppedLeaf is the audit record of one dropped leaf.
type droppedLeaf struct {
	Kind   string
	Reason string
	Len    int
	Hash   string
	Sample string
}

func (d droppedLeaf) String() string {
	return fmt.Sprintf("%s/%s len=%d sha256=%s %q", d.Kind, d.Reason, d.Len, d.Hash, d.Sample)
}

// leafHash is a short, stable content hash for correlating a logged drop with the
// source text (12 hex chars of SHA-256).
func leafHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

// leafSample is the first junkSampleRunes runes of text, whitespace-collapsed so a
// sample stays on one log line, with "…" when truncated. Rune-safe.
func leafSample(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(s) <= junkSampleRunes {
		return s
	}
	r := []rune(s)
	return string(r[:junkSampleRunes]) + "…"
}

// structureJunkFilterApplies reports whether leaves from this parser backend that
// the sidecar classified as junk are dropped.
func (im *IngestionManager) structureJunkFilterApplies(backend string) bool {
	return slices.Contains(im.cfg.JunkFilterBackends, backend)
}

// applyStructureJunkFilter removes the classified junk leaves from sd when its
// backend is configured for filtering, logs every drop, adds the count to the
// manager's running total, and returns the number of leaves this document lost
// (including any a legacy sidecar dropped before the kernel could see them).
//
// Junk-classified leaves of an unfiltered backend are KEPT; they are reported at
// DEBUG only, so the classification stays visible without spamming INFO.
func (im *IngestionManager) applyStructureJunkFilter(ctx context.Context, documentID string, sd *StructuredDocument) int {
	if sd == nil {
		return 0
	}
	filter := im.structureJunkFilterApplies(sd.Backend)
	var dropped []droppedLeaf
	kept := sd.Nodes[:0:0]
	for _, n := range sd.Nodes {
		reason, _ := n.Meta[structMetaJunkReason].(string)
		if reason == "" || n.Kind == structNodeKindSection || n.Kind == structNodeKindDocument {
			kept = append(kept, n)
			continue
		}
		rec := droppedLeaf{Kind: n.Kind, Reason: reason, Len: len(n.Text), Hash: leafHash(n.Text), Sample: leafSample(n.Text)}
		if !filter {
			slog.DebugContext(ctx, "IngestionManager: structure leaf classified as junk but KEPT (backend not in structure_junk_filter_backends)",
				"doc", documentID, "backend", sd.Backend, "kind", rec.Kind, "reason", rec.Reason,
				"len", rec.Len, "sha256", rec.Hash, "sample", rec.Sample)
			kept = append(kept, n)
			continue
		}
		slog.DebugContext(ctx, "IngestionManager: structure junk leaf dropped",
			"doc", documentID, "backend", sd.Backend, "kind", rec.Kind, "reason", rec.Reason,
			"len", rec.Len, "sha256", rec.Hash, "sample", rec.Sample)
		dropped = append(dropped, rec)
	}
	sd.Nodes = kept

	if len(dropped) > 0 {
		summary := make([]string, 0, min(len(dropped), junkSummaryLeaves))
		for _, d := range dropped[:min(len(dropped), junkSummaryLeaves)] {
			summary = append(summary, d.String())
		}
		slog.WarnContext(ctx, "IngestionManager: structure junk filter dropped content leaves",
			"doc", documentID, "backend", sd.Backend, "dropped_leaves", len(dropped),
			"leaves", summary, "more", max(0, len(dropped)-junkSummaryLeaves),
			"config", "execution.ingestion.structure_junk_filter_backends",
			"effect", "dropped leaves are not stored and not retrievable")
	}
	// A sidecar that predates junk_mode "mark" still drops in-process and reports
	// only a count; the content is gone before it reaches the kernel.
	if sd.DroppedLeaves > 0 {
		slog.WarnContext(ctx, "IngestionManager: structure sidecar dropped content leaves itself (legacy sidecar ignoring junk_mode=mark); content not auditable",
			"doc", documentID, "backend", sd.Backend, "dropped_leaves", sd.DroppedLeaves,
			"effect", "dropped leaves are not stored and not retrievable")
	}
	total := len(dropped) + sd.DroppedLeaves
	if total > 0 {
		im.structureLeavesDropped.Add(int64(total))
	}
	return total
}

// StructureLeavesDropped is the number of structure leaves this manager has
// dropped as junk since it started (kernel-side drops plus any a legacy sidecar
// reported). Per-document counts are on the ingest-complete log lines
// ("dropped_leaves").
func (im *IngestionManager) StructureLeavesDropped() int64 {
	if im == nil {
		return 0
	}
	return im.structureLeavesDropped.Load()
}
