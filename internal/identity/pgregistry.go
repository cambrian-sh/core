package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgRegistry is the Postgres domain.PrincipalRegistry over migration
// 0032_principal_registry.sql.
type PgRegistry struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPgRegistry builds the registry and checks the schema is present, so a
// kernel on an unmigrated database finds out at boot rather than at first login.
func NewPgRegistry(ctx context.Context, pool *pgxpool.Pool) (*PgRegistry, error) {
	if pool == nil {
		return nil, errors.New("principal registry: nil pool")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN
		('principals', 'principal_identities', 'principal_aliases', 'principal_local_credentials')`).Scan(&n); err != nil {
		return nil, fmt.Errorf("principal registry: schema check: %w", err)
	}
	if n != 4 {
		return nil, fmt.Errorf("principal registry: schema missing (migration 0032_principal_registry not applied; found %d of 4 tables)", n)
	}
	return &PgRegistry{pool: pool, now: time.Now}, nil
}

const principalCols = `id, kind, display_name, email, status, source, external_id, roles, created_at, updated_at`

func scanPrincipal(row pgx.Row) (domain.Principal, error) {
	var p domain.Principal
	var kind, status, source string
	err := row.Scan(&p.ID, &kind, &p.DisplayName, &p.Email, &status, &source, &p.ExternalID, &p.Roles, &p.CreatedAt, &p.UpdatedAt)
	p.Kind, p.Status, p.Source = domain.PrincipalKind(kind), domain.PrincipalStatus(status), domain.PrincipalSource(source)
	return p, err
}

// EnsurePrincipal implements domain.PrincipalRegistry. INSERT … ON CONFLICT DO
// NOTHING makes concurrent first logins of the same person converge on one row.
func (r *PgRegistry) EnsurePrincipal(ctx context.Context, p domain.Principal) (domain.Principal, bool, error) {
	p, err := validatePrincipal(p)
	if err != nil {
		return domain.Principal{}, false, err
	}
	now := r.now().UTC()
	tag, err := r.pool.Exec(ctx, `INSERT INTO principals (`+principalCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9) ON CONFLICT (id) DO NOTHING`,
		p.ID, string(p.Kind), p.DisplayName, p.Email, string(p.Status), string(p.Source), p.ExternalID, p.Roles, now)
	if err != nil {
		return domain.Principal{}, false, fmt.Errorf("principal registry: insert %s: %w", p.ID, err)
	}
	created := tag.RowsAffected() == 1
	if !created {
		// Fill-only: an import or a later login may add a display name / email the
		// first writer did not know, but never overwrites one an admin set.
		if _, err := r.pool.Exec(ctx, `UPDATE principals SET
			display_name = CASE WHEN display_name = '' THEN $2 ELSE display_name END,
			email        = CASE WHEN email = '' THEN $3 ELSE email END,
			updated_at   = CASE WHEN (display_name = '' AND $2 <> '') OR (email = '' AND $3 <> '') THEN $4 ELSE updated_at END
			WHERE id = $1`, p.ID, p.DisplayName, p.Email, now); err != nil {
			return domain.Principal{}, false, fmt.Errorf("principal registry: fill %s: %w", p.ID, err)
		}
	}
	stored, err := r.GetPrincipal(ctx, p.ID)
	if err != nil {
		return domain.Principal{}, false, err
	}
	if stored.Kind != p.Kind {
		return stored, false, fmt.Errorf("%w: %s is %s", domain.ErrPrincipalKindClash, p.ID, stored.Kind)
	}
	return stored, created, nil
}

// GetPrincipal implements domain.PrincipalRegistry.
func (r *PgRegistry) GetPrincipal(ctx context.Context, id string) (domain.Principal, error) {
	p, err := scanPrincipal(r.pool.QueryRow(ctx, `SELECT `+principalCols+` FROM principals WHERE id = $1`,
		domain.NormalizePrincipalID(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Principal{}, domain.ErrPrincipalNotFound
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("principal registry: get: %w", err)
	}
	return p, nil
}

// ListPrincipals implements domain.PrincipalRegistry, ordered by id.
func (r *PgRegistry) ListPrincipals(ctx context.Context, f domain.PrincipalFilter) ([]domain.Principal, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+principalCols+` FROM principals
		WHERE ($1 = '' OR kind = $1) AND ($2 = '' OR status = $2) AND ($3 = '' OR source = $3)
		ORDER BY id`, string(f.Kind), string(f.Status), string(f.Source))
	if err != nil {
		return nil, fmt.Errorf("principal registry: list: %w", err)
	}
	defer rows.Close()
	var out []domain.Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *PgRegistry) updateOne(ctx context.Context, q string, args ...any) error {
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("principal registry: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrPrincipalNotFound
	}
	return nil
}

// SetStatus implements domain.PrincipalRegistry.
func (r *PgRegistry) SetStatus(ctx context.Context, id string, status domain.PrincipalStatus) error {
	if _, err := validatePrincipal(domain.Principal{ID: "x", Status: status}); err != nil {
		return err
	}
	return r.updateOne(ctx, `UPDATE principals SET status = $2, updated_at = $3 WHERE id = $1`,
		domain.NormalizePrincipalID(id), string(status), r.now().UTC())
}

// SetRoles implements domain.PrincipalRegistry.
func (r *PgRegistry) SetRoles(ctx context.Context, id string, roles []string) error {
	return r.updateOne(ctx, `UPDATE principals SET roles = $2, updated_at = $3 WHERE id = $1`,
		domain.NormalizePrincipalID(id), normalizeRoles(roles), r.now().UTC())
}

func isFKViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

// LinkIdentity implements domain.PrincipalRegistry.
func (r *PgRegistry) LinkIdentity(ctx context.Context, ident domain.ExternalIdentity) error {
	if strings.TrimSpace(ident.Issuer) == "" || strings.TrimSpace(ident.Subject) == "" {
		return fmt.Errorf("%w: identity needs issuer and subject", domain.ErrInvalidPrincipal)
	}
	pid := domain.NormalizePrincipalID(ident.PrincipalID)
	_, err := r.pool.Exec(ctx, `INSERT INTO principal_identities (issuer, subject, principal_id, email, created_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (issuer, subject) DO NOTHING`,
		ident.Issuer, ident.Subject, pid, ident.Email, r.now().UTC())
	if isFKViolation(err) {
		return domain.ErrPrincipalNotFound
	}
	if err != nil {
		return fmt.Errorf("principal registry: link identity: %w", err)
	}
	var cur string
	if err := r.pool.QueryRow(ctx, `SELECT principal_id FROM principal_identities WHERE issuer = $1 AND subject = $2`,
		ident.Issuer, ident.Subject).Scan(&cur); err != nil {
		return fmt.Errorf("principal registry: link identity: %w", err)
	}
	if cur != pid {
		return domain.ErrIdentityConflict
	}
	return nil
}

// ResolveIdentity implements domain.PrincipalRegistry.
func (r *PgRegistry) ResolveIdentity(ctx context.Context, issuer, subject string) (domain.Principal, error) {
	p, err := scanPrincipal(r.pool.QueryRow(ctx, `SELECT p.id, p.kind, p.display_name, p.email, p.status, p.source,
		p.external_id, p.roles, p.created_at, p.updated_at
		FROM principal_identities i JOIN principals p ON p.id = i.principal_id
		WHERE i.issuer = $1 AND i.subject = $2`, issuer, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Principal{}, domain.ErrIdentityNotFound
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("principal registry: resolve identity: %w", err)
	}
	return p, nil
}

// AddAlias implements domain.PrincipalRegistry.
func (r *PgRegistry) AddAlias(ctx context.Context, alias, principalID string) error {
	alias, pid := strings.TrimSpace(alias), domain.NormalizePrincipalID(principalID)
	if alias == "" {
		return fmt.Errorf("%w: empty alias", domain.ErrInvalidPrincipal)
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO principal_aliases (alias, principal_id, created_at)
		VALUES ($1,$2,$3) ON CONFLICT (alias) DO NOTHING`, alias, pid, r.now().UTC())
	if isFKViolation(err) {
		return domain.ErrPrincipalNotFound
	}
	if err != nil {
		return fmt.Errorf("principal registry: add alias: %w", err)
	}
	var cur string
	if err := r.pool.QueryRow(ctx, `SELECT principal_id FROM principal_aliases WHERE alias = $1`, alias).Scan(&cur); err != nil {
		return fmt.Errorf("principal registry: add alias: %w", err)
	}
	if cur != pid {
		return domain.ErrIdentityConflict
	}
	return nil
}

// AliasesOf implements domain.PrincipalRegistry.
func (r *PgRegistry) AliasesOf(ctx context.Context, principalID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT alias FROM principal_aliases WHERE principal_id = $1 ORDER BY alias`,
		domain.NormalizePrincipalID(principalID))
	if err != nil {
		return nil, fmt.Errorf("principal registry: aliases: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Canonical implements domain.PrincipalRegistry.
func (r *PgRegistry) Canonical(ctx context.Context, ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	var id string
	err := r.pool.QueryRow(ctx, `SELECT principal_id FROM principal_aliases WHERE alias = $1`, ref).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("principal registry: canonical: %w", err)
	}
	if bare, ok := strings.CutPrefix(ref, string(domain.PrincipalUser)+":"); ok {
		if _, err := r.GetPrincipal(ctx, bare); err == nil {
			return bare, true, nil
		} else if !errors.Is(err, domain.ErrPrincipalNotFound) {
			return "", false, err
		}
	}
	return "", false, nil
}

// KnownPrincipal implements domain.PrincipalDirectory.
func (r *PgRegistry) KnownPrincipal(ctx context.Context, id string) (bool, error) {
	p, err := r.GetPrincipal(ctx, id)
	if errors.Is(err, domain.ErrPrincipalNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.Active(), nil
}

// ── local credentials (the CredentialStore LocalAccounts uses) ──

// GetCredential implements CredentialStore.
func (r *PgRegistry) GetCredential(ctx context.Context, username string) (LocalCredential, error) {
	var c LocalCredential
	err := r.pool.QueryRow(ctx, `SELECT principal_id, username, password_hash FROM principal_local_credentials
		WHERE username = $1`, strings.TrimSpace(username)).Scan(&c.PrincipalID, &c.Username, &c.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalCredential{}, ErrNoCredential
	}
	if err != nil {
		return LocalCredential{}, fmt.Errorf("principal registry: credential: %w", err)
	}
	return c, nil
}

// PutCredential implements CredentialStore (upsert by principal).
func (r *PgRegistry) PutCredential(ctx context.Context, c LocalCredential) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO principal_local_credentials (principal_id, username, password_hash, updated_at)
		VALUES ($1,$2,$3,$4) ON CONFLICT (principal_id) DO UPDATE
		SET username = EXCLUDED.username, password_hash = EXCLUDED.password_hash, updated_at = EXCLUDED.updated_at`,
		domain.NormalizePrincipalID(c.PrincipalID), strings.TrimSpace(c.Username), c.PasswordHash, r.now().UTC())
	if isFKViolation(err) {
		return domain.ErrPrincipalNotFound
	}
	if err != nil {
		return fmt.Errorf("principal registry: put credential: %w", err)
	}
	return nil
}

var (
	_ domain.PrincipalRegistry = (*PgRegistry)(nil)
	_ CredentialStore          = (*PgRegistry)(nil)
)
