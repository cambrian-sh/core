package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// ENG-2106: principal memory notes (ADR-0139 D14, migration 0030) are one of the stores
// the erasure cascade reaches. A person's "remember this for me" facts live there, so a
// forget that skips them leaves the fact in the next run's notes. The unit is one entry
// (one line, domain.MemoryNoteEntryID); erasing it, inside the one erase transaction:
//
//   - removes the line from the live document as a new revision (op "erase"), so every
//     etag held by an agent or the operator conflicts and re-reads;
//   - scrubs the same line from EVERY history row of the document (memory_notes_history
//     keeps the whole text of each revision, so leaving it there would keep the fact),
//     and clears a history reason that quotes it;
//   - records a content-free tombstone per entry (its id and the line's sha256).
//
// The rest of the document is untouched. Who may name which entry is decided by the
// chokepoint (authz.EnforcingEraser), never here.

// Store names of the memory-notes erasure counts.
const (
	eraseMemoryNotes        = "memory_notes"
	eraseMemoryNotesHistory = "memory_notes_history"
)

// noteDoc groups the entry ids named in one notes document.
type noteDoc struct {
	key     memorynotes.Key
	digests map[string]string // digest prefix → entry id
}

// docID is the document id an entry's candidate and tombstone carry: the notes
// document's decision-point resource id (memorynotes.resourceFor).
func (d noteDoc) docID() string { return "memory-notes:" + d.key.String() }

// groupNoteIDs groups entry ids by document, in (scope, owner) order. Ids that
// domain.MemoryNoteEntryID cannot have produced name nothing and are dropped.
func groupNoteIDs(ids []string) []noteDoc {
	byKey := map[memorynotes.Key]*noteDoc{}
	for _, id := range ids {
		scope, owner, digest, ok := domain.ParseMemoryNoteEntryID(id)
		if !ok {
			continue
		}
		k := memorynotes.Key{Scope: memorynotes.Scope(scope), OwnerID: owner}
		d := byKey[k]
		if d == nil {
			d = &noteDoc{key: k, digests: map[string]string{}}
			byKey[k] = d
		}
		d.digests[digest] = id
	}
	out := make([]noteDoc, 0, len(byKey))
	for _, d := range byKey {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key.String() < out[j].key.String() })
	return out
}

// resolveMemoryNotes lists the named entries that are in their live document now:
// content-free candidates carrying the document's classification (owner fence
// included), with no writer of record — a notes document is co-edited by the person's
// agents and the operator, so the chokepoint authorizes an agent on the run principal,
// not on a writer (authz.EnforcingEraser).
func resolveMemoryNotes(ctx context.Context, q pgxQuerier, ids []string) ([]domain.ErasureCandidate, error) {
	docs := groupNoteIDs(ids)
	if len(docs) == 0 {
		return nil, nil
	}
	if ok, err := tableExists(ctx, q, "memory_notes"); err != nil || !ok {
		return nil, err
	}
	var out []domain.ErasureCandidate
	for _, d := range docs {
		var content string
		err := q.QueryRow(ctx, `SELECT content FROM memory_notes WHERE scope = $1 AND owner_id = $2 AND NOT deleted`,
			string(d.key.Scope), d.key.OwnerID).Scan(&content)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("erasure resolve: memory notes: %w", err)
		}
		present := map[string]bool{}
		for _, e := range memorynotes.Entries(content) {
			present[domain.MemoryNoteEntryDigest(e.Text)[:16]] = true
		}
		var named []string
		for dg, id := range d.digests {
			if present[dg] {
				named = append(named, id)
			}
		}
		sort.Strings(named)
		for _, id := range named {
			out = append(out, domain.ErasureCandidate{
				Kind: domain.ErasureTargetMemoryNote, ID: id, Via: id, DocumentID: d.docID(),
				Tags: memorynotes.ResourceTags(d.key),
			})
		}
	}
	return out, nil
}

// eraseMemoryNotes erases the named entries inside run.tx. matched reports whether any
// named entry was in its live document.
func (s *PgErasureStore) eraseMemoryNotes(ctx context.Context, run *erasureRun, ids []string, actor string) (bool, error) {
	docs := groupNoteIDs(ids)
	if len(docs) == 0 {
		return false, nil
	}
	if ok, err := tableExists(ctx, run.tx, "memory_notes"); err != nil || !ok {
		return false, err
	}
	matched := false
	for _, d := range docs {
		var (
			content  string
			revision int64
		)
		// The row lock serializes against a concurrent notes write (whose
		// compare-and-swap then fails on the bumped revision and re-reads).
		err := run.tx.QueryRow(ctx, `SELECT content, revision FROM memory_notes
			WHERE scope = $1 AND owner_id = $2 AND NOT deleted FOR UPDATE`,
			string(d.key.Scope), d.key.OwnerID).Scan(&content, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("erase memory notes: %w", err)
		}
		// Only entries in the live document are erased (and tombstoned): the same set
		// Resolve answered, re-read under the lock.
		drop := map[string]bool{}
		texts := map[string]string{}
		for _, e := range memorynotes.Entries(content) {
			dg := domain.MemoryNoteEntryDigest(e.Text)[:16]
			if _, named := d.digests[dg]; named {
				drop[dg] = true
				texts[dg] = e.Text
			}
		}
		if len(drop) == 0 {
			continue
		}
		next, _ := memorynotes.RemoveEntries(content, drop)
		if err := run.exec(ctx, eraseMemoryNotes, `UPDATE memory_notes SET content = $3, revision = revision + 1,
				deleted = $4, updated_at = now(), updated_by = $5
			WHERE scope = $1 AND owner_id = $2`,
			string(d.key.Scope), d.key.OwnerID, next, strings.TrimSpace(next) == "", actor); err != nil {
			return false, err
		}
		// Every earlier revision: the line out of its text, and a reason that quotes
		// any erased line cleared (an agent's free-text reason can repeat the fact).
		rows, err := run.tx.Query(ctx, `SELECT revision, content, reason FROM memory_notes_history
			WHERE scope = $1 AND owner_id = $2 ORDER BY revision FOR UPDATE`, string(d.key.Scope), d.key.OwnerID)
		if err != nil {
			return false, fmt.Errorf("erase memory notes history: %w", err)
		}
		type histRow struct {
			rev             int64
			content, reason string
		}
		var scrub []histRow
		for rows.Next() {
			var h histRow
			if err := rows.Scan(&h.rev, &h.content, &h.reason); err != nil {
				rows.Close()
				return false, err
			}
			newContent, n := memorynotes.RemoveEntries(h.content, drop)
			newReason := h.reason
			for _, t := range texts {
				if strings.Contains(h.reason, t) {
					newReason = ""
					break
				}
			}
			if n > 0 || newReason != h.reason {
				scrub = append(scrub, histRow{rev: h.rev, content: newContent, reason: newReason})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		for _, h := range scrub {
			if err := run.exec(ctx, eraseMemoryNotesHistory, `UPDATE memory_notes_history SET content = $4, reason = $5
				WHERE scope = $1 AND owner_id = $2 AND revision = $3`,
				string(d.key.Scope), d.key.OwnerID, h.rev, h.content, h.reason); err != nil {
				return false, err
			}
		}
		// The erase's own revision, content-free beyond what the document still holds.
		if _, err := run.tx.Exec(ctx, `INSERT INTO memory_notes_history (scope, owner_id, revision, op, actor, reason, content, at)
			VALUES ($1, $2, $3, 'erase', $4, '', $5, now()) ON CONFLICT (scope, owner_id, revision) DO NOTHING`,
			string(d.key.Scope), d.key.OwnerID, revision+1, actor, next); err != nil {
			return false, fmt.Errorf("erase memory notes: history row: %w", err)
		}
		dgs := make([]string, 0, len(drop))
		for dg := range drop {
			dgs = append(dgs, dg)
		}
		sort.Strings(dgs)
		for _, dg := range dgs {
			run.target(domain.ErasureTargetMemoryNote, d.digests[dg], d.docID(), domain.MemoryNoteEntryDigest(texts[dg]))
		}
		matched = true
	}
	return matched, nil
}
