package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Labels (ENG-2170, owner decision 2026-11-19: a SEPARATE LABEL FIELD).
//
// Classification tags answer "who may read this"; labels answer "what is this". A
// label is a writer-set, non-classification marker — a document revision's lifecycle
// (`in-force` / `superseded`), its revision (`rev3`), the corpus and document ids it
// belongs to. They exist because those markers used to ride in the tag list, where
// the write chokepoint treats every term as a requested classification: under a
// write ceiling they were silently dropped (before ENG-2115) and are now refused
// (ENG-2115 fail-closed classification), so the `superseded` marker the ENG-2158
// ranking reads never reached a stored row.
//
// Rules that keep labels out of access control:
//   - the write chokepoints (authz.EnforcingStoreWriter, memory.EnforcingDocumentStore)
//     rewrite only metadata["tags"]; they never read, narrow or refuse a label;
//   - no TagPredicate, rowfence rule or SQL scope filter reads MetaLabels;
//   - a label lives in the row it describes (chunk metadata and documents.metadata),
//     so erasure and forget remove it with that row and nothing else holds a copy.

// MetaLabels is the metadata key that carries a row's labels: on every chunk
// (memory_items.metadata) and on the document row (documents.metadata). One key for
// both, so a reader of either needs one name.
const MetaLabels = "labels"

// Default label bounds (execution.ingestion.label_max_count / label_max_chars).
const (
	DefaultLabelMaxCount = 32
	DefaultLabelMaxChars = 128
)

// NormalizeLabels is the one validation both ingest planes apply to a request's
// labels: each is trimmed; an empty one is dropped; duplicates (exact, after the
// trim) are dropped keeping the first; a control character is refused (a newline in
// a label would let a value forge lines wherever labels are shown); more than
// maxCount distinct labels, or one longer than maxChars characters, is refused rather
// than cut — a truncated `superseded` is a different word. maxCount/maxChars <= 0
// means the default. The error names no field; callers map it to INVALID_ARGUMENT.
func NormalizeLabels(labels []string, maxCount, maxChars int) ([]string, error) {
	if maxCount <= 0 {
		maxCount = DefaultLabelMaxCount
	}
	if maxChars <= 0 {
		maxChars = DefaultLabelMaxChars
	}
	var out []string
	seen := map[string]bool{}
	for i, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		for _, r := range l {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("label %d: control character %U is not allowed", i, r)
			}
		}
		if n := utf8.RuneCountInString(l); n > maxChars {
			return nil, fmt.Errorf("label %d: %d characters exceeds the limit of %d", i, n, maxChars)
		}
		seen[l] = true
		out = append(out, l)
	}
	if len(out) > maxCount {
		return nil, fmt.Errorf("%d labels exceeds the limit of %d", len(out), maxCount)
	}
	return out, nil
}

// LabelsOf reads a row's labels from its metadata, handling both the []string and
// the []any encodings (a JSON round-trip produces the latter). Nil when none.
func LabelsOf(meta map[string]any) []string {
	if meta == nil {
		return nil
	}
	switch v := meta[MetaLabels].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
