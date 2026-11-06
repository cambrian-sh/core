// Package memorynotes is ADR-0139 D14: principal memory notes.
//
// Each principal (scope "personal") and each org (scope "org") has one editable
// markdown document — an AGENTS.md / MEMORY.md analogue — whose "## " headings are
// its sections. A run sees the notes as a FROZEN snapshot taken at run start (D3):
// writes during the run are durable at once but appear from the next run, which
// keeps the prompt cache intact. A write is bounded (budget in Unicode code points)
// and injection-scanned; an over-budget or flagged write is REFUSED, never trimmed,
// and nothing is ever evicted to make room.
//
// Storage decision (T-20261105-adr-0139-d14-principal-memory-notes-bounded): notes
// live in their own small tables (migration 0030_memory_notes.sql), not in the
// vector memory store. They are bounded MUTABLE documents addressed by
// (scope, owner) with an etag compare-and-swap, not retrieval chunks: they are
// never searched, never embedded and never ranked, so the AGENTS.md §2 tiers
// (which exist to guard retrieval and by-id chunk reads) have nothing to filter.
// The C8 authorization is applied here instead, at the one service every surface
// goes through: the structural owner rule (a run reads and writes only its own
// principal's notes and its configured org's) AND the domain.Authorizer decision
// point (premium policy can deny either). An unreadable document is reported
// exactly like an absent one.
package memorynotes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Scope is whose notes a document is.
type Scope string

const (
	// ScopePersonal is the run principal's own notes (the "sub" of the run, not the agent).
	ScopePersonal Scope = "personal"
	// ScopeOrg is the principal's org scope (ADR-0139 D9 org profile).
	ScopeOrg Scope = "org"
)

// ParseScope validates an operator-plane scope string.
func ParseScope(s string) (Scope, error) {
	switch Scope(strings.ToLower(strings.TrimSpace(s))) {
	case ScopePersonal:
		return ScopePersonal, nil
	case ScopeOrg:
		return ScopeOrg, nil
	}
	return "", fmt.Errorf("%w: scope %q (want personal | org)", ErrInvalid, s)
}

// Key addresses one document.
type Key struct {
	Scope   Scope
	OwnerID string
}

func (k Key) String() string { return string(k.Scope) + ":" + k.OwnerID }

func (k Key) validate() error {
	if k.Scope != ScopePersonal && k.Scope != ScopeOrg {
		return fmt.Errorf("%w: scope %q", ErrInvalid, k.Scope)
	}
	if strings.TrimSpace(k.OwnerID) == "" {
		return fmt.Errorf("%w: owner_id is required", ErrInvalid)
	}
	return nil
}

// Document is one stored notes document. Revision counts every write (including a
// delete), so an etag never repeats for a (scope, owner) even after a delete and a
// re-create. A deleted document keeps its row (Deleted = true, Content = "") so the
// revision sequence — and the history keyed by it — stays deterministic (AGENTS.md
// §2.6); every read path treats it as absent.
type Document struct {
	Key
	Content   string
	Revision  int64
	Deleted   bool
	UpdatedAt time.Time
	UpdatedBy string
}

// Exists reports whether the document is live (written and not deleted).
func (d Document) Exists() bool { return d.Revision > 0 && !d.Deleted }

// Size is the stored size in Unicode code points.
func (d Document) Size() int { return utf8.RuneCountInString(d.Content) }

// ETag is the opaque version of the document: the revision plus a content digest.
// "" for a document that does not exist.
func (d Document) ETag() string {
	if !d.Exists() {
		return ""
	}
	sum := sha256.Sum256([]byte(d.Content))
	return strconv.FormatInt(d.Revision, 10) + "-" + hex.EncodeToString(sum[:6])
}

// WriteMeta is who wrote and why; recorded in the history row of every write.
type WriteMeta struct {
	Actor  string
	Reason string
	Op     string
	At     time.Time
}

// ListFilter narrows a listing; empty fields match everything.
type ListFilter struct {
	Scope   Scope
	OwnerID string
}

// HistoryEntry is one write as the store recorded it.
type HistoryEntry struct {
	Key
	Revision int64
	Op       string
	Actor    string
	Reason   string
	// Content is the document AFTER the write; for a delete, the text that was
	// cleared, so a delete is recoverable by an operator.
	Content string
	At      time.Time
}

// Store is the persistence port. Implementations: the Postgres adapter
// (production, migration 0030) and the in-memory store (tests, no-DB boots).
type Store interface {
	// Get returns the document, or a zero Document (Revision 0) when it was never
	// written. A deleted document is returned with Deleted = true.
	Get(ctx context.Context, key Key) (Document, error)
	// List returns the LIVE documents matching f, ordered by (scope, owner_id).
	List(ctx context.Context, f ListFilter) ([]Document, error)
	// Put stores content as revision expectRevision+1, iff the current revision is
	// expectRevision (0 = never written). Otherwise ErrRevisionConflict. The
	// history row is written in the same transaction.
	Put(ctx context.Context, key Key, content string, expectRevision int64, w WriteMeta) (Document, error)
	// Delete marks the document deleted as revision expectRevision+1 under the same
	// compare-and-swap, recording the cleared text in history.
	Delete(ctx context.Context, key Key, expectRevision int64, w WriteMeta) (Document, error)
	// GetBudget returns the budget row for (scope, owner) exactly — owner "" is the
	// scope default — and whether one exists.
	GetBudget(ctx context.Context, scope Scope, owner string) (int, bool, error)
	// SetBudget upserts the budget row; ClearBudget removes it.
	SetBudget(ctx context.Context, scope Scope, owner string, budget int) error
	ClearBudget(ctx context.Context, scope Scope, owner string) error
}

// Sentinel errors.
var (
	// ErrInvalid is a request-shape fault (INVALID_ARGUMENT on the wire).
	ErrInvalid = errors.New("memory notes: invalid request")
	// ErrNotFound is an unknown OR unreadable document (fail-closed, indistinguishable).
	ErrNotFound = errors.New("memory notes: not found")
	// ErrRevisionConflict is a lost compare-and-swap in the store.
	ErrRevisionConflict = errors.New("memory notes: revision conflict")
)

// Rejection is why a write was refused. Strings match the operator plane's
// SetMemoryNotesOpResponse.rejection vocabulary.
type Rejection string

const (
	RejectNone            Rejection = ""
	RejectOverBudget      Rejection = "over_budget"
	RejectInjection       Rejection = "injection_flagged"
	RejectScopeDenied     Rejection = "scope_denied"
	RejectConflict        Rejection = "conflict"
	RejectSectionNotFound Rejection = "section_not_found"
)

// Op is an agent-plane edit.
type Op string

const (
	OpAppend         Op = "append"
	OpReplaceSection Op = "replace_section"
	OpRemoveSection  Op = "remove_section"
)
