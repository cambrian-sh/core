package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgToolGrantStore is the durable domain.ToolGrantStore over the 0052_tool_grants
// table (ENG-1116): the tool grants an operator wrote, so they survive a kernel
// restart. Schema comes from the migration runner; this adapter never creates
// tables. Rows are keyed by the natural key (agent_id, tool) (AGENTS.md §2.6).
// ReplaceGrants rewrites one agent's set in one transaction under a per-agent
// advisory lock, so two kernels writing the same agent serialise instead of
// interleaving their deletes and inserts.
type PgToolGrantStore struct {
	pool *pgxpool.Pool
}

var _ domain.ToolGrantStore = (*PgToolGrantStore)(nil)

// NewPgToolGrantStore wraps the shared pool.
func NewPgToolGrantStore(pool *pgxpool.Pool) *PgToolGrantStore {
	return &PgToolGrantStore{pool: pool}
}

// Ready reports whether the 0052 table exists.
func (s *PgToolGrantStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'tool_grants'`).Scan(&n)
	return err == nil && n == 1
}

// GrantsFor implements domain.GrantsProvider. An empty agentID yields none
// (fail-closed); a query error is returned, never an empty set.
func (s *PgToolGrantStore) GrantsFor(ctx context.Context, agentID string) ([]domain.ToolGrant, error) {
	if agentID == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT tool, policy FROM tool_grants WHERE agent_id = $1 ORDER BY tool`, agentID)
	if err != nil {
		return nil, fmt.Errorf("tool grants: query %q: %w", agentID, err)
	}
	defer rows.Close()
	var out []domain.ToolGrant
	for rows.Next() {
		g, err := scanToolGrant(rows, agentID)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tool grants: rows %q: %w", agentID, err)
	}
	return out, nil
}

// AllGrants implements domain.ToolGrantStore.
func (s *PgToolGrantStore) AllGrants(ctx context.Context) (map[string][]domain.ToolGrant, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, tool, policy FROM tool_grants ORDER BY agent_id, tool`)
	if err != nil {
		return nil, fmt.Errorf("tool grants: query all: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.ToolGrant{}
	for rows.Next() {
		var agent, tool string
		var policy []byte
		if err := rows.Scan(&agent, &tool, &policy); err != nil {
			return nil, fmt.Errorf("tool grants: scan: %w", err)
		}
		g, err := decodeToolGrant(agent, tool, policy)
		if err != nil {
			return nil, err
		}
		out[agent] = append(out[agent], g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tool grants: rows: %w", err)
	}
	return out, nil
}

// ReplaceGrants implements domain.ToolGrantStore: grants become the agent's whole
// operator-held set. A row that keeps its tool keeps granted_at; updated_at moves.
func (s *PgToolGrantStore) ReplaceGrants(ctx context.Context, agentID string, grants []domain.ToolGrant) error {
	if agentID == "" {
		return errors.New("tool grants: agent id is required")
	}
	tools := make([]string, 0, len(grants))
	policies := make([][]byte, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		if g.Tool == "" {
			return fmt.Errorf("tool grants: %q: empty tool name", agentID)
		}
		if seen[g.Tool] {
			continue
		}
		seen[g.Tool] = true
		p, err := json.Marshal(g.Policy)
		if err != nil {
			return fmt.Errorf("tool grants: %q/%q: encode policy: %w", agentID, g.Tool, err)
		}
		tools = append(tools, g.Tool)
		policies = append(policies, p)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tool grants: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tool-grants:' || $1, 0))`, agentID); err != nil {
		return fmt.Errorf("tool grants: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tool_grants WHERE agent_id = $1 AND NOT (tool = ANY($2::text[]))`, agentID, tools); err != nil {
		return fmt.Errorf("tool grants: revoke: %w", err)
	}
	for i, tool := range tools {
		if _, err := tx.Exec(ctx, `INSERT INTO tool_grants (agent_id, tool, policy) VALUES ($1, $2, $3::jsonb)
			ON CONFLICT (agent_id, tool) DO UPDATE SET policy = EXCLUDED.policy, updated_at = now()
			WHERE tool_grants.policy IS DISTINCT FROM EXCLUDED.policy`, agentID, tool, string(policies[i])); err != nil {
			return fmt.Errorf("tool grants: upsert %q/%q: %w", agentID, tool, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tool grants: commit: %w", err)
	}
	return nil
}

func scanToolGrant(row pgx.Row, agentID string) (domain.ToolGrant, error) {
	var tool string
	var policy []byte
	if err := row.Scan(&tool, &policy); err != nil {
		return domain.ToolGrant{}, fmt.Errorf("tool grants: scan %q: %w", agentID, err)
	}
	return decodeToolGrant(agentID, tool, policy)
}

func decodeToolGrant(agentID, tool string, policy []byte) (domain.ToolGrant, error) {
	g := domain.ToolGrant{Tool: tool}
	if len(policy) > 0 {
		if err := json.Unmarshal(policy, &g.Policy); err != nil {
			return domain.ToolGrant{}, fmt.Errorf("tool grants: %q/%q: decode policy: %w", agentID, tool, err)
		}
	}
	return g, nil
}
