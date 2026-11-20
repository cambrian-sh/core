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

// ENG-2147 (owner decision 2026-11-19): the subject-wide compliance erase of a person's
// whole personal memory notes, and the operator-journal scrub both note units share.
// See the file comment of erasure_memory_notes.go.

// scrubNotesAudit rewrites, inside run.tx, the operator_audit rows of the notes document
// key whose journalled text (before_json: the text a DeleteMemoryNotes cleared) or
// reason edit changes. A kernel without the operator_audit table (its audit store is in
// memory) skips the step.
func scrubNotesAudit(ctx context.Context, run *erasureRun, key memorynotes.Key, edit func(before, reason string) (string, string)) error {
	if ok, err := tableExists(ctx, run.tx, "operator_audit"); err != nil || !ok {
		return err
	}
	rows, err := run.tx.Query(ctx, `SELECT id, before_json, reason FROM operator_audit
		WHERE target_type = $1 AND target_id = $2 ORDER BY id FOR UPDATE`, operatorAuditNotesTarget, key.String())
	if err != nil {
		return fmt.Errorf("erase %s: %w", eraseOperatorAudit, err)
	}
	type auditRow struct{ id, before, reason string }
	var change []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.id, &a.before, &a.reason); err != nil {
			rows.Close()
			return err
		}
		before, reason := edit(a.before, a.reason)
		if before != a.before || reason != a.reason {
			change = append(change, auditRow{id: a.id, before: before, reason: reason})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range change {
		if err := run.exec(ctx, eraseOperatorAudit, `UPDATE operator_audit SET before_json = $2, reason = $3 WHERE id = $1`,
			a.id, a.before, a.reason); err != nil {
			return err
		}
	}
	return nil
}

// personalNotesKey is the document a memory_notes_owner selector names: the person's
// PERSONAL notes (org notes belong to no data subject).
func personalNotesKey(owner string) memorynotes.Key {
	return memorynotes.Key{Scope: memorynotes.ScopePersonal, OwnerID: owner}
}

// notesOwnerHasText reports whether the person's personal notes still hold any text a
// subject-wide erase would remove: the live document, a history revision's text or
// reason, or an operator delete's journalled text.
func notesOwnerHasText(ctx context.Context, q pgxQuerier, key memorynotes.Key, hasAudit bool) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory_notes WHERE scope = $1 AND owner_id = $2 AND content <> '')
		OR EXISTS (SELECT 1 FROM memory_notes_history WHERE scope = $1 AND owner_id = $2 AND (content <> '' OR reason <> ''))`,
		string(key.Scope), key.OwnerID).Scan(&ok); err != nil || ok || !hasAudit {
		return ok, err
	}
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operator_audit WHERE target_type = $1 AND target_id = $2 AND before_json <> '')`,
		operatorAuditNotesTarget, key.String()).Scan(&ok)
	return ok, err
}

// resolveMemoryNoteOwners lists one content-free memory_notes_owner candidate per named
// person whose personal notes still hold text (notesOwnerHasText) or who still has
// tool-call arguments to erase (toolArgsHeld, ENG-2254), carrying the
// document's classification (owner fence included). Only an operator or the kernel may
// erase one (authz.EnforcingEraser).
func resolveMemoryNoteOwners(ctx context.Context, q pgxQuerier, owners []string) ([]domain.ErasureCandidate, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	hasNotes, err := tableExists(ctx, q, "memory_notes")
	if err != nil {
		return nil, err
	}
	hasAudit, err := tableExists(ctx, q, "operator_audit")
	if err != nil {
		return nil, err
	}
	var out []domain.ErasureCandidate
	for _, owner := range domain.SortedUniqueIDs(owners) {
		if strings.TrimSpace(owner) == "" {
			continue
		}
		key := personalNotesKey(owner)
		ok := false
		if hasNotes {
			if ok, err = notesOwnerHasText(ctx, q, key, hasAudit); err != nil {
				return nil, fmt.Errorf("erasure resolve: memory notes owner: %w", err)
			}
		}
		if !ok {
			// ENG-2254: the person's tool-call arguments are theirs to erase too,
			// notes or no notes (erasure_tool_args.go).
			if ok, err = toolArgsHeld(ctx, q, owner); err != nil {
				return nil, fmt.Errorf("erasure resolve: memory notes owner: tool arguments: %w", err)
			}
		}
		if !ok {
			continue
		}
		out = append(out, domain.ErasureCandidate{
			Kind: domain.ErasureTargetMemoryNotesOwner, ID: owner, Via: owner,
			DocumentID: noteDoc{key: key}.docID(), Tags: memorynotes.ResourceTags(key),
		})
	}
	return out, nil
}

// eraseMemoryNoteOwners erases, inside run.tx, the whole personal notes document of each
// named person: the live text (a new content-free "erase" revision, so every held etag
// conflicts), the text and reason of every history row, and the cleared text of every
// operator delete of it; one memory_notes_owner tombstone per person and one memory_note
// tombstone (content digest) per distinct line any of those copies held. matched
// reports whether any named person's notes held text.
func (s *PgErasureStore) eraseMemoryNoteOwners(ctx context.Context, run *erasureRun, owners []string, actor string) (bool, error) {
	if len(owners) == 0 {
		return false, nil
	}
	if ok, err := tableExists(ctx, run.tx, "memory_notes"); err != nil || !ok {
		return false, err
	}
	matched := false
	for _, owner := range domain.SortedUniqueIDs(owners) {
		if strings.TrimSpace(owner) == "" {
			continue
		}
		key := personalNotesKey(owner)
		lines := map[string]string{} // entry id → line, every copy
		collect := func(content string) {
			for _, e := range memorynotes.Entries(content) {
				lines[domain.MemoryNoteEntryID(string(key.Scope), key.OwnerID, e.Text)] = e.Text
			}
		}
		var (
			content  string
			revision int64
		)
		// The row lock serializes against a concurrent notes write (whose compare-and-
		// swap then fails on the bumped revision and re-reads), as for one entry.
		err := run.tx.QueryRow(ctx, `SELECT content, revision FROM memory_notes
			WHERE scope = $1 AND owner_id = $2 FOR UPDATE`, string(key.Scope), key.OwnerID).Scan(&content, &revision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("erase memory notes owner: %w", err)
		}
		live := strings.TrimSpace(content) != ""
		collect(content)

		held := live
		rows, err := run.tx.Query(ctx, `SELECT content FROM memory_notes_history
			WHERE scope = $1 AND owner_id = $2 AND (content <> '' OR reason <> '') FOR UPDATE`, string(key.Scope), key.OwnerID)
		if err != nil {
			return false, fmt.Errorf("erase memory notes owner history: %w", err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return false, err
			}
			collect(c)
			held = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		// The operator's journal of this document: first every soft delete's cleared
		// text joins the lines, then that text goes, and so does a command reason that
		// quotes any line (the operator's own words otherwise stay: the journal's, not
		// the person's).
		if err := scrubNotesAudit(ctx, run, key, func(before, reason string) (string, string) {
			if before != "" {
				collect(before)
				held = true
			}
			return before, reason
		}); err != nil {
			return false, err
		}
		if err := scrubNotesAudit(ctx, run, key, func(before, reason string) (string, string) {
			for _, line := range lines {
				if strings.Contains(reason, line) {
					return "", ""
				}
			}
			return "", reason
		}); err != nil {
			return false, err
		}
		if !held {
			continue
		}
		if err := run.exec(ctx, eraseMemoryNotesHistory, `UPDATE memory_notes_history SET content = '', reason = ''
			WHERE scope = $1 AND owner_id = $2 AND (content <> '' OR reason <> '')`, string(key.Scope), key.OwnerID); err != nil {
			return false, err
		}
		if live {
			if err := run.exec(ctx, eraseMemoryNotes, `UPDATE memory_notes SET content = '', revision = revision + 1,
					deleted = true, updated_at = now(), updated_by = $3
				WHERE scope = $1 AND owner_id = $2`, string(key.Scope), key.OwnerID, actor); err != nil {
				return false, err
			}
			if _, err := run.tx.Exec(ctx, `INSERT INTO memory_notes_history (scope, owner_id, revision, op, actor, reason, content, at)
				VALUES ($1, $2, $3, 'erase', $4, '', '', now()) ON CONFLICT (scope, owner_id, revision) DO NOTHING`,
				string(key.Scope), key.OwnerID, revision+1, actor); err != nil {
				return false, fmt.Errorf("erase memory notes owner: history row: %w", err)
			}
		}
		docID := noteDoc{key: key}.docID()
		run.target(domain.ErasureTargetMemoryNotesOwner, owner, docID, "")
		ids := make([]string, 0, len(lines))
		for id := range lines {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			run.target(domain.ErasureTargetMemoryNote, id, docID, domain.MemoryNoteEntryDigest(lines[id]))
		}
		matched = true
	}
	return matched, nil
}
