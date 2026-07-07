// Package memory holds the runtime's working / episodic / mnemonic
// memory primitives. ChunkRelations and SiblingContext carry
// per-chunk relation metadata: parent entity ID, linear neighbor IDs,
// and sibling context (parent title/summary/scene + preceding/following
// snippets). The 512B total budget keeps the metadata column from
// ballooning under the 100 docs/min target (ADR-0060 D4).
package memory

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	parentTitleMaxBytes      = 80
	parentSummaryMaxBytes    = 120
	parentSceneMaxBytes      = 120
	precedingSnippetMaxBytes = 96
	followingSnippetMaxBytes = 96
	siblingContextMaxBytes   = 512
)

// ChunkRelations carries per-chunk relation metadata: the source-doc
// entity ID this chunk belongs to, the IDs of the immediately
// preceding / following chunks in the same document (linear
// neighborhood), and a small SiblingContext payload that gives the
// retriever enough parent + neighbor text to disambiguate the chunk
// without re-reading the source body. The SiblingContext marshal is
// hard-bounded at 512B (see SiblingContext.MarshalJSON); the
// identifier fields are never trimmed, so a 4 KiB parent_entity_id
// would not be capped (identifiers are not content).
type ChunkRelations struct {
	ParentEntityID   string         `json:"parent_entity_id"`
	PrecedingChunkID string         `json:"preceding_chunk_id,omitempty"`
	FollowingChunkID string         `json:"following_chunk_id,omitempty"`
	SiblingContext   SiblingContext `json:"sibling_context"`
}

// SiblingContext is the content half of the chunk_relations payload:
// a tight preview of the parent document (title, summary, scene) plus
// the first few bytes of the preceding and following chunks. Each
// field is hard-capped (see the const block above); the total
// marshaled JSON is hard-capped at 512B by SiblingContext.MarshalJSON.
type SiblingContext struct {
	ParentTitle      string `json:"parent_title"`      // <= 80B after trim
	ParentSummary    string `json:"parent_summary"`    // <= 120B
	ParentScene      string `json:"parent_scene"`      // <= 120B
	PrecedingSnippet string `json:"preceding_snippet"` // <= 96B
	FollowingSnippet string `json:"following_snippet"` // <= 96B
}

// MarshalJSON enforces the strict 512-byte total budget. The flow:
//
//  1. All-empty short-circuit: an empty SiblingContext marshals to
//     "{}" (the per-field empty strings would otherwise emit a
//     ~117B scaffold, which is wasted bytes for the all-empty case).
//  2. Per-field cap pass: each field is truncated to its per-field
//     maximum at the last word boundary (or hard-cut at the limit if
//     no boundary is found). This is the "designed fit" — 80+120+
//     120+96+96 = 512 bytes of content.
//  3. Total-budget pass: if the marshaled JSON still exceeds 512B
//     (because the JSON scaffold adds ~120B of fixed overhead), the
//     most-expendable fields are progressively halved in the
//     documented precedence (preceding_snippet, following_snippet,
//     parent_summary, parent_scene, parent_title) until the total
//     fits. The 5 per-field caps are the cap on this shrink; we
//     never grow a field back.
//
// JSON keys and the identifier fields on ChunkRelations (ParentEntityID,
// PrecedingChunkID, FollowingChunkID) are never trimmed — they are
// structural, not content. Only the 5 string values of SiblingContext
// are in the budget's scope.
func (s SiblingContext) MarshalJSON() ([]byte, error) {
	if s.ParentTitle == "" && s.ParentSummary == "" && s.ParentScene == "" &&
		s.PrecedingSnippet == "" && s.FollowingSnippet == "" {
		return []byte("{}"), nil
	}

	s.ParentTitle = truncateAtWordBoundary(s.ParentTitle, parentTitleMaxBytes)
	s.ParentSummary = truncateAtWordBoundary(s.ParentSummary, parentSummaryMaxBytes)
	s.ParentScene = truncateAtWordBoundary(s.ParentScene, parentSceneMaxBytes)
	s.PrecedingSnippet = truncateAtWordBoundary(s.PrecedingSnippet, precedingSnippetMaxBytes)
	s.FollowingSnippet = truncateAtWordBoundary(s.FollowingSnippet, followingSnippetMaxBytes)

	type siblingContextRaw SiblingContext
	data, err := json.Marshal(siblingContextRaw(s))
	if err != nil {
		return nil, err
	}
	if len(data) <= siblingContextMaxBytes {
		return data, nil
	}

	caps := []*struct {
		ptr *string
		cap int
	}{
		{&s.PrecedingSnippet, precedingSnippetMaxBytes},
		{&s.FollowingSnippet, followingSnippetMaxBytes},
		{&s.ParentSummary, parentSummaryMaxBytes},
		{&s.ParentScene, parentSceneMaxBytes},
		{&s.ParentTitle, parentTitleMaxBytes},
	}
	for iter := 0; iter < 32; iter++ {
		reduced := false
		for _, fc := range caps {
			if fc.cap > 1 {
				fc.cap /= 2
				if fc.cap < 1 {
					fc.cap = 1
				}
				*fc.ptr = truncateAtWordBoundary(*fc.ptr, fc.cap)
				reduced = true
				break
			}
		}
		if !reduced {
			break
		}
		data, err = json.Marshal(siblingContextRaw(s))
		if err != nil {
			return nil, err
		}
		if len(data) <= siblingContextMaxBytes {
			return data, nil
		}
	}
	return data, nil
}

// siblingContextTotalBytes returns the marshaled JSON byte count of s
// after budget enforcement. Returns -1 on marshal error (which should
// not occur for the field shapes in this file; the sentinel is for
// callers that want a strict "did marshal succeed" check).
func siblingContextTotalBytes(s SiblingContext) int {
	data, err := s.MarshalJSON()
	if err != nil {
		return -1
	}
	return len(data)
}

// truncateAtWordBoundary truncates s to at most maxBytes bytes,
// preferring a cut at the last ASCII space within the first maxBytes
// bytes (the "word boundary"). If no space is found, it hard-cuts at
// maxBytes, backing off to a valid UTF-8 rune boundary so multi-byte
// runes are never split (the ADR-0060 D4 "never truncated mid-rune"
// rule).
func truncateAtWordBoundary(s string, maxBytes int) string {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	if i := strings.LastIndexByte(truncated, ' '); i > 0 {
		return truncated[:i]
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
