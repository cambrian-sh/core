package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Memory-note entries as erasure targets (ENG-2106, ADR-0137 D2 with ADR-0139 D14).
//
// A principal memory-notes document is one mutable markdown document per (scope,
// owner); the unit a person asks to forget is one fact in it — one line ("Locker code:
// …"). An entry is therefore named by the document it lives in and a digest of its
// text, never by a position or a clock (AGENTS.md §2.6): the same line has the same id
// in every revision, so a preview token stays valid while unrelated lines change, and
// a retried erase of the same entry is the same request (ADR-0137 D6).

// MemoryNoteEntryPrefix starts every memory-note entry id.
const MemoryNoteEntryPrefix = "memory-note:"

// MemoryNoteEntryDigest is the full sha256 hex of an entry's trimmed text: the
// tombstone's content digest (content-free, ADR-0137 D1).
func MemoryNoteEntryDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

// MemoryNoteEntryID is the id of the entry whose (trimmed) text is text in the notes
// document (scope, owner): "memory-note:<scope>:<owner>#<first 16 hex of its digest>".
func MemoryNoteEntryID(scope, owner, text string) string {
	return MemoryNoteEntryPrefix + scope + ":" + owner + "#" + MemoryNoteEntryDigest(text)[:16]
}

// ParseMemoryNoteEntryID splits an entry id into its document (scope, owner) and its
// digest prefix. ok is false for anything MemoryNoteEntryID cannot have produced.
func ParseMemoryNoteEntryID(id string) (scope, owner, digest string, ok bool) {
	rest, found := strings.CutPrefix(id, MemoryNoteEntryPrefix)
	if !found {
		return "", "", "", false
	}
	i := strings.LastIndexByte(rest, '#')
	if i < 0 {
		return "", "", "", false
	}
	digest = rest[i+1:]
	scope, owner, found = strings.Cut(rest[:i], ":")
	if !found || scope == "" || owner == "" || len(digest) != 16 {
		return "", "", "", false
	}
	return scope, owner, digest, true
}
