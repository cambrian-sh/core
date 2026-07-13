package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cambrian-sh/core/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scopeNotifyChannel is the PostgreSQL LISTEN/NOTIFY channel on which agent scope
// changes are broadcast to every replica. ADR-0034 (D8/R1).
const scopeNotifyChannel = "agent_scope_changed"

// PgAgentScopeStore is the authoritative, replica-visible store for agent scope
// profiles (ADR-0034 D8/D9/R1). It satisfies scope.AgentScopeStore. Cross-replica
// revocation is delivered via LISTEN/NOTIFY (Subscribe). Scope lives here — NOT in
// per-process BBolt — so a warm replica can never serve stale scope indefinitely.
type PgAgentScopeStore struct {
	pool *pgxpool.Pool
}

// NewPgAgentScopeStore creates the agent_scopes table (idempotent) and returns the
// store. Pass the pool from PgVectorAdapter.Pool() to reuse the shared connection.
func NewPgAgentScopeStore(ctx context.Context, pool *pgxpool.Pool) (*PgAgentScopeStore, error) {
	s := &PgAgentScopeStore{pool: pool}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS agent_scopes (
			agent_id   TEXT PRIMARY KEY,
			profile    JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);`); err != nil {
		return nil, fmt.Errorf("agent_scopes ensureSchema: %w", err)
	}
	// ADR-0035 C2: per-agent operator-configured write classification.
	if _, err := pool.Exec(ctx, `
		ALTER TABLE agent_scopes ADD COLUMN IF NOT EXISTS write_tags JSONB NOT NULL DEFAULT '[]'::jsonb;`); err != nil {
		return nil, fmt.Errorf("agent_scopes write_tags migration: %w", err)
	}
	return s, nil
}

// LoadAllWriteTags returns every agentID→DefaultWriteTags for boot-time warming.
func (s *PgAgentScopeStore) LoadAllWriteTags(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, write_tags FROM agent_scopes;`)
	if err != nil {
		return nil, mapError("agent_scopes LoadAllWriteTags", err)
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, mapError("agent_scopes LoadAllWriteTags scan", err)
		}
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			return nil, fmt.Errorf("agent_scopes write_tags unmarshal %s: %w", id, err)
		}
		out[id] = tags
	}
	return out, rows.Err()
}

// GetWriteTags returns one agent's DefaultWriteTags; found==false ⇒ unknown agent.
func (s *PgAgentScopeStore) GetWriteTags(ctx context.Context, agentID string) ([]string, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT write_tags FROM agent_scopes WHERE agent_id = $1;`, agentID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, mapError("agent_scopes GetWriteTags", err)
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, false, fmt.Errorf("agent_scopes write_tags unmarshal: %w", err)
	}
	return tags, true, nil
}

// SaveWriteTags upserts an agent's DefaultWriteTags and broadcasts invalidation.
// Preserves any existing profile (the row may be created by either Save path).
func (s *PgAgentScopeStore) SaveWriteTags(ctx context.Context, agentID string, tags []string) error {
	raw, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("agent_scopes SaveWriteTags marshal: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO agent_scopes (agent_id, profile, write_tags, updated_at)
		VALUES ($1, '{}'::jsonb, $2::jsonb, now())
		ON CONFLICT (agent_id) DO UPDATE SET write_tags = EXCLUDED.write_tags, updated_at = now();`,
		agentID, string(raw)); err != nil {
		return mapError("agent_scopes SaveWriteTags", err)
	}
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2);`, scopeNotifyChannel, agentID); err != nil {
		return mapError("agent_scopes notify", err)
	}
	return nil
}

// LoadAll returns every agentID→ScopeConfig for boot-time cache warming.
func (s *PgAgentScopeStore) LoadAll(ctx context.Context) (map[string]domain.ScopeConfig, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, profile FROM agent_scopes;`)
	if err != nil {
		return nil, mapError("agent_scopes LoadAll", err)
	}
	defer rows.Close()

	out := make(map[string]domain.ScopeConfig)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, mapError("agent_scopes LoadAll scan", err)
		}
		var cfg domain.ScopeConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("agent_scopes LoadAll unmarshal %s: %w", id, err)
		}
		out[id] = cfg
	}
	return out, rows.Err()
}

// Get returns one agent's scope; found==false means the agentID is unknown.
func (s *PgAgentScopeStore) Get(ctx context.Context, agentID string) (domain.ScopeConfig, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT profile FROM agent_scopes WHERE agent_id = $1;`, agentID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ScopeConfig{}, false, nil
		}
		return domain.ScopeConfig{}, false, mapError("agent_scopes Get", err)
	}
	var cfg domain.ScopeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return domain.ScopeConfig{}, false, fmt.Errorf("agent_scopes Get unmarshal: %w", err)
	}
	return cfg, true, nil
}

// Save upserts the agent's scope and broadcasts an invalidation to all replicas
// via NOTIFY. The caller (ScopeResolver.SaveScope) validates before calling.
func (s *PgAgentScopeStore) Save(ctx context.Context, agentID string, cfg domain.ScopeConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("agent_scopes Save marshal: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO agent_scopes (agent_id, profile, updated_at)
		VALUES ($1, $2::jsonb, now())
		ON CONFLICT (agent_id) DO UPDATE SET profile = EXCLUDED.profile, updated_at = now();`,
		agentID, string(raw)); err != nil {
		return mapError("agent_scopes Save", err)
	}
	// Cross-replica revocation: every replica's Subscribe loop receives agentID.
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2);`, scopeNotifyChannel, agentID); err != nil {
		return mapError("agent_scopes notify", err)
	}
	return nil
}

// Subscribe acquires a dedicated connection, LISTENs on the scope-change channel,
// and emits each changed agentID on the returned channel until ctx is cancelled.
// Wire the channel into ScopeResolver.WatchInvalidations for cross-replica
// revocation. ADR-0034 (R1).
func (s *PgAgentScopeStore) Subscribe(ctx context.Context) (<-chan string, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, mapError("agent_scopes Subscribe acquire", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+scopeNotifyChannel+";"); err != nil {
		conn.Release()
		return nil, mapError("agent_scopes LISTEN", err)
	}

	out := make(chan string, 16)
	go func() {
		defer close(out)
		defer conn.Release()
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				return // ctx cancelled or connection dropped
			}
			select {
			case out <- n.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
