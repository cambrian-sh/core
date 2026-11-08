package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ApprovalState is an approval's lifecycle state. pending is the only live state;
// every other state is terminal and published as a resolution event.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalDenied   ApprovalState = "denied"
	// ApprovalExpired: nobody answered within the window — fail-closed.
	ApprovalExpired ApprovalState = "expired"
	// ApprovalWithdrawn: the requester stopped waiting (its turn ended, the call
	// was cancelled) — fail-closed.
	ApprovalWithdrawn ApprovalState = "withdrawn"
	// ApprovalNeedsHierarchy: the only approver rules are relationship rules and
	// no hierarchy is installed to resolve them (stage 2) — fail-closed, visibly.
	ApprovalNeedsHierarchy ApprovalState = "needs_hierarchy"
	// ApprovalUnroutable: no rule resolved, or no surface that could reach an
	// eligible approver is connected — fail-closed (today's "no subscriber").
	ApprovalUnroutable ApprovalState = "unroutable"
)

// Terminal reports whether s is a resolution.
func (s ApprovalState) Terminal() bool { return s != ApprovalPending && s != "" }

// ApprovalItem is one durable approval: the request, the route the policy
// resolved for it, and its lifecycle. Persisted on raise and on resolution
// (ADR-0142 D11: pending approvals are durable).
type ApprovalItem struct {
	// ID is deterministic from the natural key and its round (AGENTS.md §2.6):
	// a retried request after a restart finds the same row, and an id never
	// repeats across boots the way appr-<counter> did.
	ID string
	// Kind is the wire kind ("" = dangerous tool, skill_access, schedule,
	// machine_consent, choose_machine, account_confirm, publish, …).
	Kind string
	// ActionClass is the policy class (tool, payment, public_record_release, …).
	ActionClass string
	// Resource + Scope (+ ActionClass) key a standing approval.
	Resource string
	Scope    string

	AgentID     string
	ToolName    string
	ArgsPreview string
	Object      string
	Machine     string
	Candidates  []string
	// Args is the call's full argument JSON (ApprovalRequest.Args), available to
	// ApprovalPolicy.Route at raise time and NOT persisted: the route the policy
	// resolved from it is stored on the item (Rules), so the rule shown and the
	// rule enforced stay one value after a restart.
	Args []byte

	// Beneficiary is the person the action is for — the subject of a self rule.
	Beneficiary    PrincipalRef
	ConversationID string
	TaskID         string
	SessionID      string

	Rules         []ApprovalRule
	AllowStanding bool
	NeverSelf     bool
	// Async marks an item no caller blocks on; a registered kind handler applies
	// its effect when it is approved (account confirmation, publish).
	Async bool

	State      ApprovalState
	RaisedAt   time.Time
	ExpiresAt  time.Time
	ResolvedAt time.Time
	ResolvedBy string
	// Choice is a choose_machine answer.
	Choice string
	// StandingID names the standing approval that pre-approved this item.
	StandingID string
	// Detail explains a resolution (refusal reason, handler failure, …).
	Detail string
	// DuplicateNotice is ApprovalRequest.DuplicateNotice: what earlier call of the
	// conversation this one may duplicate, shown to the approver. The Postgres
	// store has no column for it; the hub keeps it in memory for the life of the
	// pending item, as long-lived as the in-memory history it was computed from.
	DuplicateNotice string

	NaturalKey string
	Round      int
	// Consumed marks a decided item a requester has taken. A decision recorded
	// while no requester was waiting (after a restart) is handed to the next
	// identical request instead of being asked again.
	Consumed bool
}

// RuleSummary renders the item's approver rules for display.
func (i ApprovalItem) RuleSummary() string { return RulesString(i.Rules) }

// StandingApproval pre-approves one (action class, resource, scope) until
// revoked or expired (owner acceptance 2026-11-06 item 1). Recorded, listable,
// revocable, audited.
type StandingApproval struct {
	// ID is deterministic from (class, resource, scope): re-granting replaces.
	ID          string
	ActionClass string
	Resource    string
	Scope       string
	Beneficiary PrincipalRef
	GrantedBy   Approver
	GrantedAt   time.Time
	// ExpiresAt zero ⇒ until revoked.
	ExpiresAt time.Time
	RevokedAt time.Time
	RevokedBy string
	Reason    string
	// FromApproval is the approval whose answer granted it.
	FromApproval string
	LastUsedAt   time.Time
	UseCount     int
}

// Active reports whether the standing approval applies at t.
func (s StandingApproval) Active(t time.Time) bool {
	if !s.RevokedAt.IsZero() {
		return false
	}
	return s.ExpiresAt.IsZero() || t.Before(s.ExpiresAt)
}

// StandingApprovalID is the deterministic id of a standing approval key.
func StandingApprovalID(class, resource, scope string) string {
	return "stand-" + approvalHash(class, resource, scope)
}

// ApprovalID is the deterministic id of an approval's (natural key, round).
func ApprovalID(naturalKey string, round int) string {
	return "apr-" + approvalHash(naturalKey, strconv.Itoa(round))
}

func approvalHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// ApprovalQuery filters ListApprovals. Zero values mean "any".
type ApprovalQuery struct {
	States []ApprovalState
	Limit  int
}

// StandingQuery filters ListStanding.
type StandingQuery struct {
	// Scope restricts to one scope ("" = all).
	Scope string
	// IncludeInactive also returns revoked and expired grants.
	IncludeInactive bool
}

// ErrApprovalNotFound: no approval with that id.
var ErrApprovalNotFound = errors.New("approval not found")

// ErrStandingNotFound: no standing approval with that id.
var ErrStandingNotFound = errors.New("standing approval not found")

// ApprovalStore persists approvals and standing approvals. Implementations:
// MemoryApprovalStore (tests, and kernels with no database) and the Postgres
// adapter (internal/infrastructure/postgres, migration 0033).
type ApprovalStore interface {
	PutApproval(ctx context.Context, it ApprovalItem) error
	// ResolveApproval moves a PENDING item to it.State (compare-and-set). false ⇒
	// it was no longer pending; nothing changed.
	ResolveApproval(ctx context.Context, it ApprovalItem) (bool, error)
	// MarkConsumed records that a requester took a decided item.
	MarkConsumed(ctx context.Context, id string) error
	GetApproval(ctx context.Context, id string) (ApprovalItem, error)
	// LatestByKey returns the highest-round item for a natural key (false = none).
	LatestByKey(ctx context.Context, naturalKey string) (ApprovalItem, bool, error)
	ListApprovals(ctx context.Context, q ApprovalQuery) ([]ApprovalItem, error)

	PutStanding(ctx context.Context, s StandingApproval) error
	GetStanding(ctx context.Context, id string) (StandingApproval, error)
	ListStanding(ctx context.Context, q StandingQuery) ([]StandingApproval, error)
}

// MemoryApprovalStore is the in-process ApprovalStore. Durable only for the life
// of the process; the kernel uses the Postgres store whenever a database exists.
type MemoryApprovalStore struct {
	mu       sync.Mutex
	items    map[string]ApprovalItem
	standing map[string]StandingApproval
}

// NewMemoryApprovalStore returns an empty store.
func NewMemoryApprovalStore() *MemoryApprovalStore {
	return &MemoryApprovalStore{items: map[string]ApprovalItem{}, standing: map[string]StandingApproval{}}
}

var _ ApprovalStore = (*MemoryApprovalStore)(nil)

func cloneItem(it ApprovalItem) ApprovalItem {
	it.Candidates = append([]string(nil), it.Candidates...)
	it.Rules = append([]ApprovalRule(nil), it.Rules...)
	it.Args = nil // never persisted (parity with the Postgres store)
	return it
}

func (m *MemoryApprovalStore) PutApproval(_ context.Context, it ApprovalItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[it.ID] = cloneItem(it)
	return nil
}

func (m *MemoryApprovalStore) ResolveApproval(_ context.Context, it ApprovalItem) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.items[it.ID]
	if !ok {
		return false, ErrApprovalNotFound
	}
	if cur.State != ApprovalPending {
		return false, nil
	}
	m.items[it.ID] = cloneItem(it)
	return true, nil
}

func (m *MemoryApprovalStore) MarkConsumed(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.items[id]
	if !ok {
		return ErrApprovalNotFound
	}
	cur.Consumed = true
	m.items[id] = cur
	return nil
}

func (m *MemoryApprovalStore) GetApproval(_ context.Context, id string) (ApprovalItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return ApprovalItem{}, ErrApprovalNotFound
	}
	return cloneItem(it), nil
}

func (m *MemoryApprovalStore) LatestByKey(_ context.Context, key string) (ApprovalItem, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best ApprovalItem
	found := false
	for _, it := range m.items {
		if it.NaturalKey == key && (!found || it.Round > best.Round) {
			best, found = it, true
		}
	}
	return cloneItem(best), found, nil
}

func (m *MemoryApprovalStore) ListApprovals(_ context.Context, q ApprovalQuery) ([]ApprovalItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[ApprovalState]bool{}
	for _, s := range q.States {
		want[s] = true
	}
	var out []ApprovalItem
	for _, it := range m.items {
		if len(want) > 0 && !want[it.State] {
			continue
		}
		out = append(out, cloneItem(it))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RaisedAt.Equal(out[j].RaisedAt) {
			return out[i].RaisedAt.After(out[j].RaisedAt)
		}
		return out[i].ID < out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *MemoryApprovalStore) PutStanding(_ context.Context, s StandingApproval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.GrantedBy.Roles = append([]string(nil), s.GrantedBy.Roles...)
	m.standing[s.ID] = s
	return nil
}

func (m *MemoryApprovalStore) GetStanding(_ context.Context, id string) (StandingApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.standing[id]
	if !ok {
		return StandingApproval{}, ErrStandingNotFound
	}
	return s, nil
}

func (m *MemoryApprovalStore) ListStanding(_ context.Context, q StandingQuery) ([]StandingApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []StandingApproval
	for _, s := range m.standing {
		if q.Scope != "" && s.Scope != q.Scope {
			continue
		}
		if !q.IncludeInactive && !s.Active(now) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
