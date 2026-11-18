package memorynotes

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// Entries: the unit of a forget inside a notes document (ENG-2106).
//
// A person asks to forget one fact ("my locker code"), and a notes document holds many
// — so the erasure unit is one ENTRY: one non-blank line that is not a "## " heading or
// a code-fence marker, together with the section it sits in. It is named by
// domain.MemoryNoteEntryID (document + digest of the line), erased by the one erasure
// chokepoint (MemoryStack.Eraser: the line leaves the live document and every history
// row), and found for a free-text forget by MatchEntries.

// Entry is one erasable line of a notes document.
type Entry struct {
	// Text is the line, trimmed.
	Text string
	// Section is the heading of the section the line is in ("" in the preamble).
	Section string
}

// Entries lists a document's entries in document order. Two identical lines are two
// entries with one id: erasing it removes both.
func Entries(content string) []Entry {
	var out []Entry
	walkLines(content, func(line, section string, isHeading, isFence bool) {
		if t := strings.TrimSpace(line); t != "" && !isHeading && !isFence {
			out = append(out, Entry{Text: t, Section: section})
		}
	})
	return out
}

// walkLines calls fn for every line of content (newline kept) with the section heading
// it belongs to. Headings inside fenced code blocks are content, as in splitBlocks.
func walkLines(content string, fn func(line, section string, isHeading, isFence bool)) {
	if content == "" {
		return
	}
	section, inFence := "", false
	for _, ln := range strings.SplitAfter(content, "\n") {
		if ln == "" {
			continue
		}
		trimmed := strings.TrimRight(ln, "\r\n")
		fence := strings.TrimLeft(trimmed, " ")
		isFence := strings.HasPrefix(fence, "```") || strings.HasPrefix(fence, "~~~")
		if isFence {
			inFence = !inFence
		}
		isHeading := false
		if !inFence && !isFence {
			if name, ok := headingName(trimmed); ok {
				section, isHeading = name, true
			}
		}
		fn(ln, section, isHeading, isFence)
	}
}

// RemoveEntries removes every entry line whose digest prefix (the part of a
// domain.MemoryNoteEntryID after '#') is in digests, drops a section heading left with
// no entry by the removal, and collapses the blank lines the removal left behind.
// removed counts the lines removed (headings excluded). The content is returned
// unchanged when nothing matched.
func RemoveEntries(content string, digests map[string]bool) (string, int) {
	type ln struct {
		text          string
		heading, drop bool
		blank         bool
		sec           int
	}
	var lines []ln
	sec := -1
	removed := 0
	walkLines(content, func(line, _ string, isHeading, isFence bool) {
		l := ln{text: line, heading: isHeading, blank: strings.TrimSpace(line) == ""}
		if isHeading {
			sec = len(lines)
		}
		l.sec = sec
		if !l.blank && !isHeading && !isFence && digests[domain.MemoryNoteEntryDigest(line)[:16]] {
			l.drop = true
			removed++
		}
		lines = append(lines, l)
	})
	if removed == 0 {
		return content, 0
	}
	// A heading whose section lost entries and kept none goes too: a lone heading can
	// itself name the fact ("## Locker code").
	for h := range lines {
		if !lines[h].heading {
			continue
		}
		lost, kept := false, false
		for j := h + 1; j < len(lines) && lines[j].sec == h; j++ {
			if lines[j].blank {
				continue
			}
			if lines[j].drop {
				lost = true
			} else {
				kept = true
			}
		}
		if lost && !kept {
			lines[h].drop = true
		}
	}
	var b strings.Builder
	prevBlank := true // no leading blank lines
	for _, l := range lines {
		if l.drop {
			continue
		}
		if l.blank {
			if prevBlank {
				continue
			}
			prevBlank = true
		} else {
			prevBlank = false
		}
		b.WriteString(l.text)
	}
	out := strings.TrimRight(b.String(), "\r\n \t")
	if out == "" {
		return "", removed
	}
	return out + "\n", removed
}

// queryTerms are the words a free-text forget must find in an entry: letter/digit runs,
// lower-cased; words shorter than minTermRunes ("my", "is") are dropped unless the
// query has nothing else. A tokenizer detail, not a policy: matching itself requires
// EVERY remaining term (MatchEntries).
const minTermRunes = 3

func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func queryTerms(query string) []string {
	all := tokens(query)
	var out []string
	for _, t := range all {
		if utf8.RuneCountInString(t) >= minTermRunes {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// MatchEntries returns the entries a free-text forget names: those whose words (the
// line's and its section heading's) contain EVERY query term, each as a word or the
// start of one ("şifre" finds "şifresi", "locker" finds "Locker"). Over-matching is the
// riskier failure for a deletion (owner decision 2026-11-17, ENG-1953), so a partial
// match never qualifies; notes are never embedded, so there is no cosine to floor on.
// The agent sees its notes in its instructions and can query with their own words.
func MatchEntries(entries []Entry, query string) []Entry {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil
	}
	var out []Entry
	for _, e := range entries {
		words := tokens(e.Section + " " + e.Text)
		all := true
		for _, t := range terms {
			found := false
			for _, w := range words {
				if strings.HasPrefix(w, t) {
					found = true
					break
				}
			}
			if !found {
				all = false
				break
			}
		}
		if all {
			out = append(out, e)
		}
	}
	return out
}

// ResourceTags is the stored classification of a notes document as the erasure
// chokepoint sees it: the same tags the decision point gets for every read and write of
// it (resourceFor), the owner fence included on a personal document.
func ResourceTags(key Key) []string {
	_, tags := resourceFor(key)
	return tags
}

// EntryMatch is one entry a free-text forget found, with the id the erasure chokepoint
// resolves and its text (the person's own words, for the preview snippet).
type EntryMatch struct {
	ID   string
	Text string
}

// MatchForErase lists the entries of owner's PERSONAL notes that match query
// (MatchEntries), for the forget preview (forget.Service.PreviewQuery). It reads the
// store directly and decides nothing: every id it returns is resolved and authorized by
// the erasure chokepoint before anything is shown or erased (hidden = absent).
func (s *Service) MatchForErase(ctx context.Context, owner, query string) ([]EntryMatch, error) {
	if s == nil || strings.TrimSpace(owner) == "" {
		return nil, nil
	}
	key := Key{Scope: ScopePersonal, OwnerID: owner}
	d, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("memory notes: get %s: %w", key, err)
	}
	if !d.Exists() {
		return nil, nil
	}
	var out []EntryMatch
	seen := map[string]bool{}
	for _, e := range MatchEntries(Entries(d.Content), query) {
		id := domain.MemoryNoteEntryID(string(key.Scope), key.OwnerID, e.Text)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, EntryMatch{ID: id, Text: e.Text})
	}
	return out, nil
}

// DropSnapshots forgets every cached run snapshot of owner, so a run that already froze
// its notes re-reads them on its next read after an erasure (ENG-2106): the frozen
// snapshot (D3) is a cache of content, and erased content must not be served from it.
func (s *Service) DropSnapshots(owner string) {
	if s == nil || owner == "" {
		return
	}
	suffix := "\x00" + owner
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.snaps {
		if strings.HasSuffix(k, suffix) {
			delete(s.snaps, k)
		}
	}
}
