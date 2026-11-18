package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgApprovalStore is the durable domain.ApprovalStore over the 0033_approvals
// tables (ADR-0142 D11). Schema comes from the migration runner; this adapter
// never creates tables. Approval answers are compare-and-set on state, so two
// surfaces answering at once cannot both win.
type PgApprovalStore struct {
	pool *pgxpool.Pool
}

var _ domain.ApprovalStore = (*PgApprovalStore)(nil)

// NewPgApprovalStore wraps the shared pool.
func NewPgApprovalStore(pool *pgxpool.Pool) *PgApprovalStore { return &PgApprovalStore{pool: pool} }

// Ready reports whether the 0033 tables exist.
func (s *PgApprovalStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('approvals','standing_approvals')`).Scan(&n)
	return err == nil && n == 2
}

type ruleJSON struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

func apprEncodeRules(rs []domain.ApprovalRule) []byte {
	out := make([]ruleJSON, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleJSON{Kind: string(r.Kind), Value: r.Value})
	}
	b, _ := json.Marshal(out)
	return b
}

func apprDecodeRules(b []byte) []domain.ApprovalRule {
	var in []ruleJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return nil
	}
	out := make([]domain.ApprovalRule, 0, len(in))
	for _, r := range in {
		out = append(out, domain.ApprovalRule{Kind: domain.ApprovalRuleKind(r.Kind), Value: r.Value})
	}
	return out
}

func apprEncodeStrings(xs []string) []byte {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return b
}

func apprDecodeStrings(b []byte) []string {
	var out []string
	_ = json.Unmarshal(b, &out)
	return out
}

func apprNullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func apprFromNullTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

const approvalCols = `id, natural_key, round, kind, action_class, resource, scope, agent_id, tool_name, args_preview,
	object, machine, candidates, beneficiary_id, beneficiary_kind, conversation_id, task_id, session_id, rules,
	allow_standing, never_self, async, state, raised_at, expires_at, resolved_at, resolved_by, choice, standing_id,
	detail, consumed, intent`

func approvalArgs(it domain.ApprovalItem) []any {
	return []any{it.ID, it.NaturalKey, it.Round, it.Kind, it.ActionClass, it.Resource, it.Scope, it.AgentID,
		it.ToolName, it.ArgsPreview, it.Object, it.Machine, apprEncodeStrings(it.Candidates), it.Beneficiary.ID,
		string(it.Beneficiary.Kind), it.ConversationID, it.TaskID, it.SessionID, apprEncodeRules(it.Rules),
		it.AllowStanding, it.NeverSelf, it.Async, string(it.State), it.RaisedAt.UTC(), apprNullTime(it.ExpiresAt),
		apprNullTime(it.ResolvedAt), it.ResolvedBy, it.Choice, it.StandingID, it.Detail, it.Consumed,
		apprEncodeIntent(it.Intent)}
}

// apprEncodeIntent is the 0059 intent column: JSON text, or NULL.
func apprEncodeIntent(in *domain.ApprovalIntent) *string {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil // strings and raw JSON always marshal
	}
	s := string(b)
	return &s
}

func apprDecodeIntent(s *string) (*domain.ApprovalIntent, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	var in domain.ApprovalIntent
	if err := json.Unmarshal([]byte(*s), &in); err != nil {
		return nil, fmt.Errorf("approvals: intent: %w", err)
	}
	return &in, nil
}

func scanApproval(row pgx.Row) (domain.ApprovalItem, error) {
	var it domain.ApprovalItem
	var cands, rules []byte
	var benKind, state string
	var expires, resolved *time.Time
	var intent *string
	err := row.Scan(&it.ID, &it.NaturalKey, &it.Round, &it.Kind, &it.ActionClass, &it.Resource, &it.Scope,
		&it.AgentID, &it.ToolName, &it.ArgsPreview, &it.Object, &it.Machine, &cands, &it.Beneficiary.ID, &benKind,
		&it.ConversationID, &it.TaskID, &it.SessionID, &rules, &it.AllowStanding, &it.NeverSelf, &it.Async, &state,
		&it.RaisedAt, &expires, &resolved, &it.ResolvedBy, &it.Choice, &it.StandingID, &it.Detail, &it.Consumed, &intent)
	if err != nil {
		return it, err
	}
	// An unreadable intent is an error, not a silent nil (AGENTS.md §3.2): a
	// nil intent would refuse the approve, so say why instead.
	if it.Intent, err = apprDecodeIntent(intent); err != nil {
		return it, err
	}
	it.Beneficiary.Kind = domain.PrincipalKind(benKind)
	it.Candidates = apprDecodeStrings(cands)
	it.Rules = apprDecodeRules(rules)
	it.State = domain.ApprovalState(state)
	it.ExpiresAt, it.ResolvedAt = apprFromNullTime(expires), apprFromNullTime(resolved)
	return it, nil
}

// PutApproval upserts one item.
func (s *PgApprovalStore) PutApproval(ctx context.Context, it domain.ApprovalItem) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO approvals (`+approvalCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)
		ON CONFLICT (id) DO UPDATE SET
			kind = EXCLUDED.kind, action_class = EXCLUDED.action_class, resource = EXCLUDED.resource,
			scope = EXCLUDED.scope, agent_id = EXCLUDED.agent_id, tool_name = EXCLUDED.tool_name,
			args_preview = EXCLUDED.args_preview, object = EXCLUDED.object, machine = EXCLUDED.machine,
			candidates = EXCLUDED.candidates, beneficiary_id = EXCLUDED.beneficiary_id,
			beneficiary_kind = EXCLUDED.beneficiary_kind, conversation_id = EXCLUDED.conversation_id,
			task_id = EXCLUDED.task_id, session_id = EXCLUDED.session_id, rules = EXCLUDED.rules,
			allow_standing = EXCLUDED.allow_standing, never_self = EXCLUDED.never_self, async = EXCLUDED.async,
			state = EXCLUDED.state, raised_at = EXCLUDED.raised_at, expires_at = EXCLUDED.expires_at,
			resolved_at = EXCLUDED.resolved_at, resolved_by = EXCLUDED.resolved_by, choice = EXCLUDED.choice,
			standing_id = EXCLUDED.standing_id, detail = EXCLUDED.detail, consumed = EXCLUDED.consumed,
			intent = EXCLUDED.intent`,
		approvalArgs(it)...)
	if err != nil {
		return fmt.Errorf("approvals: put: %w", err)
	}
	return nil
}

// ResolveApproval CASes a pending item to its terminal state.
func (s *PgApprovalStore) ResolveApproval(ctx context.Context, it domain.ApprovalItem) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET state = $2, resolved_at = $3, resolved_by = $4, choice = $5,
		standing_id = $6, detail = $7, consumed = $8, intent = $9 WHERE id = $1 AND state = 'pending'`,
		it.ID, string(it.State), apprNullTime(it.ResolvedAt), it.ResolvedBy, it.Choice, it.StandingID, it.Detail, it.Consumed,
		apprEncodeIntent(it.Intent))
	if err != nil {
		return false, fmt.Errorf("approvals: resolve: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM approvals WHERE id = $1)`, it.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("approvals: resolve: %w", err)
	}
	if !exists {
		return false, domain.ErrApprovalNotFound
	}
	return false, nil
}

// MarkConsumed records that a requester took a decided item.
func (s *PgApprovalStore) MarkConsumed(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET consumed = TRUE WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("approvals: consume: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrApprovalNotFound
	}
	return nil
}

// GetApproval reads one item.
func (s *PgApprovalStore) GetApproval(ctx context.Context, id string) (domain.ApprovalItem, error) {
	it, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ApprovalItem{}, domain.ErrApprovalNotFound
	}
	if err != nil {
		return domain.ApprovalItem{}, fmt.Errorf("approvals: get: %w", err)
	}
	return it, nil
}

// LatestByKey reads the highest round of a natural key.
func (s *PgApprovalStore) LatestByKey(ctx context.Context, key string) (domain.ApprovalItem, bool, error) {
	it, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE natural_key = $1 ORDER BY round DESC LIMIT 1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ApprovalItem{}, false, nil
	}
	if err != nil {
		return domain.ApprovalItem{}, false, fmt.Errorf("approvals: latest: %w", err)
	}
	return it, true, nil
}

// ListApprovals lists items, newest first.
func (s *PgApprovalStore) ListApprovals(ctx context.Context, q domain.ApprovalQuery) ([]domain.ApprovalItem, error) {
	states := make([]string, 0, len(q.States))
	for _, st := range q.States {
		states = append(states, string(st))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+approvalCols+` FROM approvals
		WHERE (cardinality($1::text[]) = 0 OR state = ANY($1::text[]))
		  AND (NOT $3 OR intent IS NOT NULL)
		ORDER BY raised_at DESC, id LIMIT $2`, states, limit, q.WithIntent)
	if err != nil {
		return nil, fmt.Errorf("approvals: list: %w", err)
	}
	defer rows.Close()
	var out []domain.ApprovalItem
	for rows.Next() {
		it, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("approvals: list: scan: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

const standingCols = `id, action_class, resource, scope, beneficiary_id, beneficiary_kind, granted_by_id, granted_by_kind,
	granted_by_roles, granted_at, expires_at, revoked_at, revoked_by, reason, from_approval, last_used_at, use_count`

func scanStanding(row pgx.Row) (domain.StandingApproval, error) {
	var s domain.StandingApproval
	var benKind, gbKind string
	var roles []byte
	var expires, revoked, used *time.Time
	err := row.Scan(&s.ID, &s.ActionClass, &s.Resource, &s.Scope, &s.Beneficiary.ID, &benKind, &s.GrantedBy.Principal.ID,
		&gbKind, &roles, &s.GrantedAt, &expires, &revoked, &s.RevokedBy, &s.Reason, &s.FromApproval, &used, &s.UseCount)
	if err != nil {
		return s, err
	}
	s.Beneficiary.Kind = domain.PrincipalKind(benKind)
	s.GrantedBy.Principal.Kind = domain.PrincipalKind(gbKind)
	s.GrantedBy.Roles = apprDecodeStrings(roles)
	s.ExpiresAt, s.RevokedAt, s.LastUsedAt = apprFromNullTime(expires), apprFromNullTime(revoked), apprFromNullTime(used)
	return s, nil
}

// PutStanding upserts a standing approval.
func (s *PgApprovalStore) PutStanding(ctx context.Context, st domain.StandingApproval) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO standing_approvals (`+standingCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (id) DO UPDATE SET
			beneficiary_id = EXCLUDED.beneficiary_id, beneficiary_kind = EXCLUDED.beneficiary_kind,
			granted_by_id = EXCLUDED.granted_by_id, granted_by_kind = EXCLUDED.granted_by_kind,
			granted_by_roles = EXCLUDED.granted_by_roles, granted_at = EXCLUDED.granted_at,
			expires_at = EXCLUDED.expires_at, revoked_at = EXCLUDED.revoked_at, revoked_by = EXCLUDED.revoked_by,
			reason = EXCLUDED.reason, from_approval = EXCLUDED.from_approval, last_used_at = EXCLUDED.last_used_at,
			use_count = EXCLUDED.use_count`,
		st.ID, st.ActionClass, st.Resource, st.Scope, st.Beneficiary.ID, string(st.Beneficiary.Kind),
		st.GrantedBy.Principal.ID, string(st.GrantedBy.Principal.Kind), apprEncodeStrings(st.GrantedBy.Roles),
		st.GrantedAt.UTC(), apprNullTime(st.ExpiresAt), apprNullTime(st.RevokedAt), st.RevokedBy, st.Reason,
		st.FromApproval, apprNullTime(st.LastUsedAt), st.UseCount)
	if err != nil {
		return fmt.Errorf("approvals: put standing: %w", err)
	}
	return nil
}

// GetStanding reads one standing approval.
func (s *PgApprovalStore) GetStanding(ctx context.Context, id string) (domain.StandingApproval, error) {
	st, err := scanStanding(s.pool.QueryRow(ctx, `SELECT `+standingCols+` FROM standing_approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.StandingApproval{}, domain.ErrStandingNotFound
	}
	if err != nil {
		return domain.StandingApproval{}, fmt.Errorf("approvals: get standing: %w", err)
	}
	return st, nil
}

// ListStanding lists standing approvals.
func (s *PgApprovalStore) ListStanding(ctx context.Context, q domain.StandingQuery) ([]domain.StandingApproval, error) {
	// $3: the instant expiry is judged at (domain.StandingQuery.At); NULL = now().
	var at *time.Time
	if !q.At.IsZero() {
		t := q.At.UTC()
		at = &t
	}
	rows, err := s.pool.Query(ctx, `SELECT `+standingCols+` FROM standing_approvals
		WHERE ($1 = '' OR scope = $1)
		  AND ($2 OR (revoked_at IS NULL AND (expires_at IS NULL OR expires_at > COALESCE($3::timestamptz, now()))))
		ORDER BY id`, q.Scope, q.IncludeInactive, at)
	if err != nil {
		return nil, fmt.Errorf("approvals: list standing: %w", err)
	}
	defer rows.Close()
	var out []domain.StandingApproval
	for rows.Next() {
		st, err := scanStanding(rows)
		if err != nil {
			return nil, fmt.Errorf("approvals: list standing: scan: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
