// Package identity is the kernel half of ADR-0142 stage 1: the principal
// registry (Postgres over migration 0032, and this in-memory twin for a kernel
// with no database and for tests), local password accounts, the single-issuer
// OIDC verifier, and the chain that composes them behind domain.Authenticator.
package identity

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// MemRegistry is the in-memory domain.PrincipalRegistry. Concurrency-safe. It
// loses everything on restart, which is why a kernel with a database uses
// PgRegistry; this one serves the database-less edge profile (where the only
// person is the env bootstrap operator, re-seeded on every boot) and tests.
type MemRegistry struct {
	mu         sync.RWMutex
	principals map[string]domain.Principal
	idents     map[string]domain.ExternalIdentity // key issuer\x00subject
	aliases    map[string]string                  // alias → principal id
	creds      map[string]LocalCredential         // username → credential
	now        func() time.Time
}

// NewMemRegistry builds an empty registry.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		principals: map[string]domain.Principal{},
		idents:     map[string]domain.ExternalIdentity{},
		aliases:    map[string]string{},
		creds:      map[string]LocalCredential{},
		now:        time.Now,
	}
}

func identKey(iss, sub string) string { return iss + "\x00" + sub }

// validatePrincipal is shared by both implementations: only persons (and the
// reserved actor kind) live in the registry, and an id is never empty.
func validatePrincipal(p domain.Principal) (domain.Principal, error) {
	p.ID = domain.NormalizePrincipalID(p.ID)
	if p.ID == "" {
		return p, fmt.Errorf("%w: empty id", domain.ErrInvalidPrincipal)
	}
	if strings.ContainsAny(p.ID, "\x00\n\r") {
		return p, fmt.Errorf("%w: id %q contains a control character", domain.ErrInvalidPrincipal, p.ID)
	}
	if p.Kind == "" {
		p.Kind = domain.PrincipalUser
	}
	if p.Kind != domain.PrincipalUser && p.Kind != domain.PrincipalActor {
		return p, fmt.Errorf("%w: kind %q is not a person kind (user, actor)", domain.ErrInvalidPrincipal, p.Kind)
	}
	if p.Status == "" {
		p.Status = domain.PrincipalActive
	}
	switch p.Status {
	case domain.PrincipalActive, domain.PrincipalSuspended, domain.PrincipalDeprovisioned:
	default:
		return p, fmt.Errorf("%w: status %q", domain.ErrInvalidPrincipal, p.Status)
	}
	if p.Source == "" {
		p.Source = domain.SourceImport
	}
	p.Roles = normalizeRoles(p.Roles)
	return p, nil
}

func normalizeRoles(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// EnsurePrincipal implements domain.PrincipalRegistry.
func (m *MemRegistry) EnsurePrincipal(_ context.Context, p domain.Principal) (domain.Principal, bool, error) {
	p, err := validatePrincipal(p)
	if err != nil {
		return domain.Principal{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.principals[p.ID]; ok {
		if cur.Kind != p.Kind {
			return cur, false, fmt.Errorf("%w: %s is %s", domain.ErrPrincipalKindClash, p.ID, cur.Kind)
		}
		changed := false
		if cur.DisplayName == "" && p.DisplayName != "" {
			cur.DisplayName, changed = p.DisplayName, true
		}
		if cur.Email == "" && p.Email != "" {
			cur.Email, changed = p.Email, true
		}
		if changed {
			cur.UpdatedAt = m.now().UTC()
			m.principals[p.ID] = cur
		}
		return cur, false, nil
	}
	now := m.now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	m.principals[p.ID] = p
	return p, true, nil
}

// GetPrincipal implements domain.PrincipalRegistry.
func (m *MemRegistry) GetPrincipal(_ context.Context, id string) (domain.Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.principals[domain.NormalizePrincipalID(id)]
	if !ok {
		return domain.Principal{}, domain.ErrPrincipalNotFound
	}
	return p, nil
}

// ListPrincipals implements domain.PrincipalRegistry, ordered by id.
func (m *MemRegistry) ListPrincipals(_ context.Context, f domain.PrincipalFilter) ([]domain.Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Principal, 0, len(m.principals))
	for _, p := range m.principals {
		if (f.Kind == "" || p.Kind == f.Kind) && (f.Status == "" || p.Status == f.Status) &&
			(f.Source == "" || p.Source == f.Source) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SetStatus implements domain.PrincipalRegistry.
func (m *MemRegistry) SetStatus(_ context.Context, id string, status domain.PrincipalStatus) error {
	if _, err := validatePrincipal(domain.Principal{ID: "x", Status: status}); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.principals[domain.NormalizePrincipalID(id)]
	if !ok {
		return domain.ErrPrincipalNotFound
	}
	p.Status, p.UpdatedAt = status, m.now().UTC()
	m.principals[p.ID] = p
	return nil
}

// SetRoles implements domain.PrincipalRegistry.
func (m *MemRegistry) SetRoles(_ context.Context, id string, roles []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.principals[domain.NormalizePrincipalID(id)]
	if !ok {
		return domain.ErrPrincipalNotFound
	}
	p.Roles, p.UpdatedAt = normalizeRoles(roles), m.now().UTC()
	m.principals[p.ID] = p
	return nil
}

// LinkIdentity implements domain.PrincipalRegistry.
func (m *MemRegistry) LinkIdentity(_ context.Context, ident domain.ExternalIdentity) error {
	if strings.TrimSpace(ident.Issuer) == "" || strings.TrimSpace(ident.Subject) == "" {
		return fmt.Errorf("%w: identity needs issuer and subject", domain.ErrInvalidPrincipal)
	}
	ident.PrincipalID = domain.NormalizePrincipalID(ident.PrincipalID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.principals[ident.PrincipalID]; !ok {
		return domain.ErrPrincipalNotFound
	}
	k := identKey(ident.Issuer, ident.Subject)
	if cur, ok := m.idents[k]; ok {
		if cur.PrincipalID != ident.PrincipalID {
			return domain.ErrIdentityConflict
		}
		return nil
	}
	ident.CreatedAt = m.now().UTC()
	m.idents[k] = ident
	return nil
}

// ResolveIdentity implements domain.PrincipalRegistry.
func (m *MemRegistry) ResolveIdentity(_ context.Context, issuer, subject string) (domain.Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ident, ok := m.idents[identKey(issuer, subject)]
	if !ok {
		return domain.Principal{}, domain.ErrIdentityNotFound
	}
	p, ok := m.principals[ident.PrincipalID]
	if !ok {
		return domain.Principal{}, domain.ErrPrincipalNotFound
	}
	return p, nil
}

// AddAlias implements domain.PrincipalRegistry.
func (m *MemRegistry) AddAlias(_ context.Context, alias, principalID string) error {
	alias, principalID = strings.TrimSpace(alias), domain.NormalizePrincipalID(principalID)
	if alias == "" {
		return fmt.Errorf("%w: empty alias", domain.ErrInvalidPrincipal)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.principals[principalID]; !ok {
		return domain.ErrPrincipalNotFound
	}
	if cur, ok := m.aliases[alias]; ok && cur != principalID {
		return domain.ErrIdentityConflict
	}
	m.aliases[alias] = principalID
	return nil
}

// AliasesOf implements domain.PrincipalRegistry.
func (m *MemRegistry) AliasesOf(_ context.Context, principalID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for a, id := range m.aliases {
		if id == principalID {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Canonical implements domain.PrincipalRegistry.
func (m *MemRegistry) Canonical(_ context.Context, ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if id, ok := m.aliases[ref]; ok {
		return id, true, nil
	}
	if id, ok := strings.CutPrefix(ref, string(domain.PrincipalUser)+":"); ok {
		if _, known := m.principals[id]; known {
			return id, true, nil
		}
	}
	return "", false, nil
}

// KnownPrincipal implements domain.PrincipalDirectory.
func (m *MemRegistry) KnownPrincipal(_ context.Context, id string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.principals[domain.NormalizePrincipalID(id)]
	return ok && p.Active(), nil
}

// GetCredential implements CredentialStore.
func (m *MemRegistry) GetCredential(_ context.Context, username string) (LocalCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.creds[strings.TrimSpace(username)]
	if !ok {
		return LocalCredential{}, ErrNoCredential
	}
	return c, nil
}

// PutCredential implements CredentialStore (one credential per person; a
// rename drops the old username).
func (m *MemRegistry) PutCredential(_ context.Context, c LocalCredential) error {
	c.PrincipalID, c.Username = domain.NormalizePrincipalID(c.PrincipalID), strings.TrimSpace(c.Username)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.principals[c.PrincipalID]; !ok {
		return domain.ErrPrincipalNotFound
	}
	if cur, ok := m.creds[c.Username]; ok && cur.PrincipalID != c.PrincipalID {
		return domain.ErrIdentityConflict
	}
	for u, cur := range m.creds {
		if cur.PrincipalID == c.PrincipalID {
			delete(m.creds, u)
		}
	}
	m.creds[c.Username] = c
	return nil
}

var (
	_ domain.PrincipalRegistry = (*MemRegistry)(nil)
	_ CredentialStore          = (*MemRegistry)(nil)
)
