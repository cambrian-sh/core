package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// The principal registry (ADR-0142 D3).
//
// Until ADR-0142 Cambrian had no principal table: a person existed because
// something REFERENCED them (a binding, a scope row, a group, a policy link —
// principal_directory.go). That cannot carry a lifecycle: you cannot suspend a
// reference, own a device with one, or address an approval to one. The registry
// is the store of record for PERSONS (kind `user`) and, later, non-human actors
// (kind `actor`, reserved — D4). Agents, daemons and machines are NOT persons and
// do not live here; they keep their own registries.
//
// It is the ONE place persons are created and resolved:
//   - local accounts (the bootstrap operator and any password user),
//   - an OIDC first login (issuer + subject → person),
//   - the one-time, idempotent import of every person the deployment already
//     references (premium: bindings, person scope rows, group members, links;
//     core: machine-token owners).
//
// Ids are DETERMINISTIC from the natural key (AGENTS.md §2.6), never a clock:
//   - an existing referenced person keeps the bare id it is already referenced
//     by (`alice`), because owner tags, policy links and bindings key on it;
//   - a local account's id is its normalised username;
//   - an OIDC person created at first login is PersonIDFor("oidc", iss, sub);
//   - a SCIM person (stage 2) will be PersonIDFor("scim", directory, externalId).
//
// The principal a person acts as is PrincipalRef{Kind: PrincipalUser, ID: id},
// rendered `user:<id>`. That is the form approvals are addressed to.
// ─────────────────────────────────────────────────────────────────────────────

// PrincipalActor is RESERVED for non-human actor principals (ADR-0142 D4): a
// persona, a brand account, a game character — a policy subject and memory owner
// with accountable `user` owners that never authenticates interactively. Nothing
// creates one yet; the kind exists so the registry schema and this enum need no
// breaking change when the first use case arrives with its own ADR.
const PrincipalActor PrincipalKind = "actor"

// PrincipalStatus is a registry person's lifecycle state.
type PrincipalStatus string

const (
	PrincipalActive        PrincipalStatus = "active"
	PrincipalSuspended     PrincipalStatus = "suspended"
	PrincipalDeprovisioned PrincipalStatus = "deprovisioned"
)

// PrincipalSource records who created (and therefore may edit) a person
// (ADR-0142 D3/D6: a SCIM-sourced person is read-only in the editor).
type PrincipalSource string

const (
	SourceLocal     PrincipalSource = "local"     // a local password account
	SourceOIDC      PrincipalSource = "oidc"      // created at an OIDC first login
	SourceSCIM      PrincipalSource = "scim"      // pushed by a directory (stage 2)
	SourceDirectory PrincipalSource = "directory" // a reconcile adapter (stage 2)
	SourceEditor    PrincipalSource = "editor"    // the built-in hierarchy editor (stage 2)
	SourceImport    PrincipalSource = "import"    // the one-time import of referenced persons
)

// Principal is one registry row.
type Principal struct {
	ID          string
	Kind        PrincipalKind // PrincipalUser, or the reserved PrincipalActor
	DisplayName string
	Email       string
	Status      PrincipalStatus
	Source      PrincipalSource
	// ExternalID is the source's own key for this person (an OIDC subject, a SCIM
	// externalId). Informational; identity resolution goes through ExternalIdentity.
	ExternalID string
	// Roles are the person's plane roles (`operator`, `viewer`, …): a SET, not the
	// old operator|viewer enum (ADR-0142 D1). What the person may READ is the
	// Authorizer's decision, never a role's.
	Roles     []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ref is the principal this person acts as.
func (p Principal) Ref() PrincipalRef { return PrincipalRef{ID: p.ID, Kind: p.Kind} }

// Active reports whether the person may act at all.
func (p Principal) Active() bool { return p.Status == "" || p.Status == PrincipalActive }

// ExternalIdentity maps an issuer's subject to a registry person. The pair
// (Issuer, Subject) is the identity — NEVER the email alone, because emails are
// reassigned (ADR-0142 threat table, impersonation).
type ExternalIdentity struct {
	Issuer      string
	Subject     string
	Email       string
	PrincipalID string
	CreatedAt   time.Time
}

// PrincipalFilter narrows List.
type PrincipalFilter struct {
	Kind   PrincipalKind
	Status PrincipalStatus
	Source PrincipalSource
}

// Sentinel errors callers must distinguish (AGENTS.md §3.2).
var (
	ErrPrincipalNotFound  = errors.New("principal registry: principal not found")
	ErrIdentityNotFound   = errors.New("principal registry: external identity not linked")
	ErrIdentityConflict   = errors.New("principal registry: external identity already linked to another principal")
	ErrPrincipalInactive  = errors.New("principal registry: principal is suspended or deprovisioned")
	ErrInvalidPrincipal   = errors.New("principal registry: invalid principal")
	ErrPrincipalKindClash = errors.New("principal registry: id already registered with another kind")
)

// PrincipalRegistry is the port (ADR-0142 D3). Core ships the implementations
// (internal/identity: Postgres over migration 0032, and an in-memory one for a
// kernel with no database); premium reads and writes through it.
type PrincipalRegistry interface {
	// EnsurePrincipal creates the person if absent and returns the stored row.
	// It is IDEMPOTENT: a second call with the same id changes nothing that was
	// already set except filling empty display name/email (so an import can run
	// on every boot). created reports whether this call inserted the row.
	EnsurePrincipal(ctx context.Context, p Principal) (stored Principal, created bool, err error)
	// GetPrincipal returns ErrPrincipalNotFound for an unknown id.
	GetPrincipal(ctx context.Context, id string) (Principal, error)
	ListPrincipals(ctx context.Context, f PrincipalFilter) ([]Principal, error)
	// SetStatus moves a person through its lifecycle.
	SetStatus(ctx context.Context, id string, status PrincipalStatus) error
	// SetRoles replaces the person's role set.
	SetRoles(ctx context.Context, id string, roles []string) error
	// UpdateProfile overwrites the person's display name and/or email (an empty
	// argument keeps the stored value). Unlike EnsurePrincipal's fill-only rule it
	// replaces a non-empty value, so it is for the source that OWNS the person's
	// profile — a directory updating a person it provisioned (owner decision
	// 2026-11-14, ENG-683: the directory wins on its own rows; locally created
	// persons keep local edits). Deciding WHO may call it is the caller's job;
	// the registry only stores. ErrPrincipalNotFound for an unknown id.
	UpdateProfile(ctx context.Context, id, displayName, email string) error

	// LinkIdentity binds (issuer, subject) to a person. Re-linking the same pair
	// to the same person is a no-op; to a different person is ErrIdentityConflict.
	LinkIdentity(ctx context.Context, ident ExternalIdentity) error
	// ResolveIdentity returns the person an (issuer, subject) is linked to, or
	// ErrIdentityNotFound.
	ResolveIdentity(ctx context.Context, issuer, subject string) (Principal, error)

	// AddAlias records that a legacy principal string (e.g. `agent:alice`, the
	// kind a bound person ran as before ADR-0142) names this person, so audit
	// queries match both forms during the migration window. Idempotent.
	AddAlias(ctx context.Context, alias, principalID string) error
	// AliasesOf returns every alias recorded for a person.
	AliasesOf(ctx context.Context, principalID string) ([]string, error)
	// Canonical maps a principal string — a canonical `user:<id>` or a recorded
	// alias — to the person's id. ok=false when it names nobody in the registry.
	Canonical(ctx context.Context, ref string) (id string, ok bool, err error)

	// KnownPrincipal implements PrincipalDirectory: a registry person exists and
	// is active. This makes act-for usable in OSS (ADR-0142 D3).
	KnownPrincipal(ctx context.Context, id string) (bool, error)
}

// PersonIDFor derives a deterministic registry id from a source and its natural
// key parts (AGENTS.md §2.6): `<source>-<first 16 hex of sha256(parts joined by NUL)>`.
// Used for persons first seen through a source that does not already name them
// by a readable id (an OIDC first login, a SCIM push).
func PersonIDFor(source string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return source + "-" + hex.EncodeToString(h[:8])
}

// NormalizePrincipalID trims a person id or username. It deliberately does NOT
// change case: existing deployments reference persons by the exact string their
// operator typed (owner tags `owner:<id>`, policy links, bindings, the env
// bootstrap user), and folding case here would silently re-key all of it.
func NormalizePrincipalID(id string) string { return strings.TrimSpace(id) }

// LegacyAgentAlias is the alias recorded for a person who ran as an agent
// principal before ADR-0142 (bindings on MCP and chat resolved to `agent:<id>`).
func LegacyAgentAlias(id string) string { return string(PrincipalAgent) + ":" + id }

// UnboundMCPClientPrefix is the id prefix of an UNBOUND MCP client's own principal
// (`agent:mcp:<client>`, mcpserve). An external app with no person behind it is an
// agent, and an owner string carrying this prefix names that agent, not a person.
const UnboundMCPClientPrefix = "mcp:"

// PersonPrincipal is THE rule that turns a person id carried by a binding, a
// machine-token owner or an act-for target into the principal the kernel runs as
// (ADR-0142 D3: a person is `user` on every door). One function, so the worker
// owner and the task beneficiary — compared with == on the full PrincipalRef
// (contribution_routing.go, tool_executor.go, consent_bridge.go) — can never
// disagree about the kind (feasibility K-7).
//
// The single exception is an owner naming an unbound MCP client's own principal
// (`mcp:<client>`): that is an agent, and a machine token issued to it before
// ADR-0142 keeps serving that agent's tasks.
func PersonPrincipal(id string) PrincipalRef {
	id = strings.TrimSpace(id)
	if id == "" {
		return PrincipalRef{}
	}
	if strings.HasPrefix(id, UnboundMCPClientPrefix) {
		return AgentPrincipal(id)
	}
	return UserPrincipal(id)
}

// PersonMayAct reports whether a person id may act right now: false only when the
// registry KNOWS the person and they are suspended or deprovisioned (ADR-0142 D5:
// deprovisioning revokes access). An id the registry has not seen yet is allowed —
// it is a legacy referenced person the import has not reached, and refusing it
// would turn an upgrade into an outage — and so is a nil registry. A registry
// that cannot be read fails CLOSED: lifecycle is an access decision.
func PersonMayAct(ctx context.Context, reg PrincipalRegistry, id string) bool {
	ok, err := PersonMayActChecked(ctx, reg, id)
	return ok && err == nil
}

// PersonMayActChecked is PersonMayAct for a caller that must tell "this person
// may not act" from "the registry could not be read": the latter is returned as
// an error wrapping ErrAuthUnavailable (with ok=false — still fail closed), so a
// door can answer it as retryable rather than as a refusal of the person
// (T-20261118-operator-rpcs-answer-unauthenticated-invalid-or).
func PersonMayActChecked(ctx context.Context, reg PrincipalRegistry, id string) (bool, error) {
	if reg == nil || id == "" {
		return true, nil
	}
	p, err := reg.GetPrincipal(ctx, id)
	if errors.Is(err, ErrPrincipalNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: principal registry: %w", ErrAuthUnavailable, err)
	}
	return p.Active(), nil
}

// PrincipalAliasReader is the slice of the registry PrincipalForms reads.
// domain.PrincipalRegistry satisfies it.
type PrincipalAliasReader interface {
	GetPrincipal(ctx context.Context, id string) (Principal, error)
	AliasesOf(ctx context.Context, principalID string) ([]string, error)
	Canonical(ctx context.Context, ref string) (id string, ok bool, err error)
}

// PrincipalForms returns every string an audit or journal row may have recorded
// for the person ref names, so a query by one form matches all of them (ADR-0142
// migration step 2): the bare id (operator audit's actor), `user:<id>` (the
// principal a person acts as today), and every recorded alias — `agent:<id>`,
// the kind a bound person ran as before ADR-0142. ref itself is always included.
//
// ref may be any of those forms. A ref the registry does not know (not a person,
// or no registry) yields just {ref}, so a query by an agent's or a daemon's id is
// unchanged. A registry error also yields {ref}: widening a READ filter on a
// guess would be worse than matching only the exact string the caller typed.
func PrincipalForms(ctx context.Context, reg PrincipalAliasReader, ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" || reg == nil {
		return []string{ref}
	}
	id, ok, err := reg.Canonical(ctx, ref)
	if err != nil {
		return []string{ref}
	}
	if !ok {
		// A bare id: Canonical maps aliases and `user:<id>` only.
		if strings.Contains(ref, ":") {
			return []string{ref}
		}
		if _, gerr := reg.GetPrincipal(ctx, ref); gerr != nil {
			return []string{ref}
		}
		id = ref
	}
	aliases, err := reg.AliasesOf(ctx, id)
	if err != nil {
		aliases = nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range append([]string{ref, id, string(PrincipalUser) + ":" + id}, aliases...) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}
