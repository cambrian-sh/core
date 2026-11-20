package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Tool-call arguments of an erased person (ENG-2254, owner decision 2026-11-20,
// option A). Two tables hold a side-effecting call's full arguments, which can
// carry the person's data: the conversation action history behind the
// approver's duplicate notice (action_history, 0066, ENG-784) and the stored
// intent of a pending sync approval (approvals.intent, 0059). Both were bounded
// in time (the notice window; the intent is cleared once its approval resolves)
// but untouched by erasure, which is meant to be immediate.
//
// The subject-wide memory_notes_owner selector — the operator's compliance erase
// of everything personal of one principal — now also reaches them, in the same
// erase transaction (one erasure pathway, AGENTS.md §2.8; no contract change):
//
//   - action_history: every row scoped to a conversation that principal owns
//     (conversations.owner_id) is deleted;
//   - approvals.intent: the intent of every PENDING approval whose call was on
//     the principal's behalf (intent on_behalf_of), in a conversation the
//     principal owns (intent conversation_owner, or the item's conversation),
//     or for the principal as beneficiary is cleared. Such an approve is then
//     refused ("nothing to carry out"), never run from a partial record.
//
// Content-free counts land in the receipt under eraseActionHistory and
// eraseApprovalIntents. No tombstone: these rows are copies of calls, not
// records a later write could resurrect.
const (
	eraseActionHistory   = "action_history"
	eraseApprovalIntents = "approval_intents"
)

// toolArgsOwnerClause selects the pending approvals with an intent that belong to
// principal $1 (see above). Shared by resolve and erase so they never disagree.
const toolArgsOwnerClause = `state = 'pending' AND intent IS NOT NULL AND (
		intent::jsonb->>'on_behalf_of' = $1 OR intent::jsonb->>'conversation_owner' = $1
		OR (beneficiary_kind = 'user' AND beneficiary_id = $1)
		OR conversation_id IN (SELECT id FROM conversations WHERE owner_id = $1))`

// toolArgsTables reports which of the two tables (and the conversations table
// the history is keyed through) this database has.
func toolArgsTables(ctx context.Context, q pgxQuerier) (history, approvals bool, err error) {
	conv, err := tableExists(ctx, q, "conversations")
	if err != nil {
		return false, false, err
	}
	if history, err = tableExists(ctx, q, "action_history"); err != nil {
		return false, false, err
	}
	if approvals, err = tableExists(ctx, q, "approvals"); err != nil {
		return false, false, err
	}
	return history && conv, approvals && conv, nil
}

// toolArgsHeld reports whether any action_history row or pending approval intent
// of owner remains — a reason, besides notes text, for a memory_notes_owner
// candidate.
func toolArgsHeld(ctx context.Context, q pgxQuerier, owner string) (bool, error) {
	history, approvals, err := toolArgsTables(ctx, q)
	if err != nil {
		return false, err
	}
	var ok bool
	if history {
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM action_history WHERE scope IN
			(SELECT 'conversation:' || id FROM conversations WHERE owner_id = $1))`, owner).Scan(&ok); err != nil || ok {
			return ok, err
		}
	}
	if approvals {
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM approvals WHERE `+toolArgsOwnerClause+`)`, owner).Scan(&ok); err != nil {
			return false, err
		}
	}
	return ok, nil
}

// eraseToolArgsOf deletes, inside run.tx, each named principal's conversation
// action history and clears the intent of their pending approvals. matched
// reports whether anything was removed.
func (s *PgErasureStore) eraseToolArgsOf(ctx context.Context, run *erasureRun, owners []string) (bool, error) {
	if len(owners) == 0 {
		return false, nil
	}
	history, approvals, err := toolArgsTables(ctx, run.tx)
	if err != nil {
		return false, fmt.Errorf("erase tool arguments: %w", err)
	}
	before := run.counts[eraseActionHistory] + run.counts[eraseApprovalIntents]
	for _, owner := range domain.SortedUniqueIDs(owners) {
		if strings.TrimSpace(owner) == "" {
			continue
		}
		if history {
			if err := run.exec(ctx, eraseActionHistory, `DELETE FROM action_history WHERE scope IN
				(SELECT 'conversation:' || id FROM conversations WHERE owner_id = $1)`, owner); err != nil {
				return false, err
			}
		}
		if approvals {
			if err := run.exec(ctx, eraseApprovalIntents,
				`UPDATE approvals SET intent = NULL WHERE `+toolArgsOwnerClause, owner); err != nil {
				return false, err
			}
		}
	}
	return run.counts[eraseActionHistory]+run.counts[eraseApprovalIntents] > before, nil
}
