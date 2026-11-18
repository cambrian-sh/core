package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Memory erasure — the principal-initiated forget of ADR-0137 (ENG-411).
//
// Erasure is a HARD delete of content and every derivative in one Postgres transaction
// (D4); what remains is a content-free record: the receipt (memory_erasures) and one
// tombstone row per erased unit (memory_erasure_targets) carrying ids, a sha256 digest
// and the erase time — never text, an embedding, a triplet string, a fact value or a
// quote (D1). It is disjoint from supersession (ADR-0136 D5): supersession closes
// intervals and never deletes; erasure deletes and never merely invalidates.
//
// Stage A (S1–S2) shipped the storage and the transaction; stage B (S3–S5) the
// authorization (authz.EnforcingEraser, built only in kernel.NewMemoryStack and reached
// only through MemoryStack.Eraser()), the no-resurrection fences (ErrSourceErased, the
// ingest origin) and the async worker (internal/memory/erasureworker). Stage C (S6–S7)
// the contract: internal/memory/forget (two-phase preview/forget) behind the agent-plane
// PreviewForget/Forget/ListMyErasures and operator-plane PreviewForgetMemory/
// ForgetMemory/ListErasures/GetErasure RPCs and the forget_memory tool, plus the
// ErasureParticipant / ErasureObserver plugin seams (erasure_contract.go).

// Errors a MemoryEraser returns. Each is distinguishable (AGENTS.md §3.2): an empty or
// malformed request is a caller error, nothing-matched is NotFound, never a silent
// success with zero counts.
var (
	// ErrInvalidErasureRequest: the request names no principal, an unknown reason or
	// principal kind, an empty selector, or an empty id.
	ErrInvalidErasureRequest = errors.New("invalid erasure request")
	// ErrNothingToErase: the selector matched no stored record (and no earlier receipt
	// of the same request exists). See NothingToEraseError for the earlier request that
	// may have erased the same ids.
	ErrNothingToErase = errors.New("nothing to erase")
	// ErrEraseDenied: the caller may not erase at least one record the selector names
	// (ADR-0137 D3); nothing was erased (all-or-nothing). Distinct from
	// ErrNothingToErase: a record the caller cannot even read is reported as absent.
	ErrEraseDenied = errors.New("erase denied")
)

// NothingToEraseError is ErrNothingToErase with the id of an earlier erase request that
// already removed at least one of the named ids (ADR-0137 D6), when one exists. Whether
// a caller may see EarlierRequestID is the enforcing layer's decision (S3).
type NothingToEraseError struct {
	EarlierRequestID string
}

func (e *NothingToEraseError) Error() string {
	if e.EarlierRequestID == "" {
		return ErrNothingToErase.Error()
	}
	return fmt.Sprintf("%s (already erased by %s)", ErrNothingToErase, e.EarlierRequestID)
}

// Is makes errors.Is(err, ErrNothingToErase) true.
func (e *NothingToEraseError) Is(target error) bool { return target == ErrNothingToErase }

// ErasurePrincipalKind says who asked for an erasure.
type ErasurePrincipalKind string

const (
	ErasureByAgent    ErasurePrincipalKind = "agent"
	ErasureByOperator ErasurePrincipalKind = "operator"
	ErasureByKernel   ErasurePrincipalKind = "kernel"
)

// ErasureReason is the closed set of reasons an erasure records (never free text: the
// receipt must stay content-free).
type ErasureReason string

const (
	// ErasureReasonUserRequest: the data subject or the writing principal asked to forget.
	ErasureReasonUserRequest ErasureReason = "user_request"
	// ErasureReasonCompliance: a legal obligation (right to erasure, KVKK/GDPR).
	ErasureReasonCompliance ErasureReason = "compliance"
	// ErasureReasonCorrection: the content was wrong or ingested by mistake.
	ErasureReasonCorrection ErasureReason = "correction"
)

// Selector kinds (memory_erasures.selector_kinds, memory_erasure_targets.target_kind).
const (
	ErasureTargetDocument   = "document"
	ErasureTargetThread     = "thread"
	ErasureTargetChunk      = "chunk"
	ErasureTargetExperience = "experience"
	ErasureTargetEntity     = "entity"
	ErasureTargetEvidence   = "evidence"
	// ErasureTargetMemoryNote is one entry (one line) of a principal memory-notes
	// document (ADR-0139 D14), named by MemoryNoteEntryID (ENG-2106).
	ErasureTargetMemoryNote = "memory_note"
	// ErasureTargetConversationMessage is an agent reply whose text an erase replaced
	// with RedactedReplyText because it rested on an erased record (ENG-2109). A
	// tombstone kind only, never a selector kind: replies are reached through what
	// they cite.
	ErasureTargetConversationMessage = "conversation_message"
)

// Erasure statuses (memory_erasures.status).
const (
	// ErasureStatusComplete: every step ran inside the transaction.
	ErasureStatusComplete = "complete"
	// ErasureStatusPendingAsync: the transaction committed (nothing a principal-serving
	// read touches holds the content any more) and outbox steps remain (D7).
	ErasureStatusPendingAsync = "pending_async"
)

// Async steps (memory_erasure_outbox.step), drained by the erasure worker (S5).
const (
	// ErasureStepCASDelete deletes one content-addressed blob (evidence bytes, a source
	// document body) by CID. The worker must first check that no surviving row still
	// references the CID: the store is content-addressed, so identical bytes ingested by
	// another document share it.
	ErasureStepCASDelete = "cas_delete"
	// ErasureStepVaultDelete deletes one artifact-vault body (by hash) of an erased
	// run-ledger entry (ADR-0157 D5.6). The worker first checks that no surviving
	// ledger entry names the same bytes.
	ErasureStepVaultDelete = "vault_delete"
)

// ErasureSelector names what to erase: concrete ids only (ADR-0137 D2). There is no
// free-text field — a natural-language forget is resolved to ids by a scoped preview
// before it reaches a MemoryEraser.
type ErasureSelector struct {
	// DocumentIDs: source documents; the cascade unit (chunks, sections, edges, KG
	// triplets, PageRank, fact items, markers, evidence, documents_legacy rows).
	DocumentIDs []string
	// ThreadIDs: ingest threads (IngestDocument.ThreadID / an upload session), resolved
	// to the documents that arrived on them.
	ThreadIDs []string
	// ChunkIDs: memory ids returned earlier. A chunk that is its document's only content
	// erases the document; otherwise the chunk and its derivatives alone.
	ChunkIDs []string
	// ExperienceIDs: ADR-0095 episode parents (their chunks cascade, D6 of ADR-0095).
	ExperienceIDs []string
	// EntityRefs: knowledge-substrate items about, or carrying an entity value equal to,
	// these refs (KnowledgeErasure). Chunks that merely mention an entity are NOT erased.
	EntityRefs []string
	// MemoryNoteIDs: entries of principal memory-notes documents (MemoryNoteEntryID,
	// ENG-2106). The entry's line is removed from the live document and scrubbed from
	// every history row of it; the rest of the document stays. An agent may name only
	// entries of its run principal's own personal notes (authz.EnforcingEraser).
	MemoryNoteIDs []string
}

// kinds returns (kind, sorted unique ids) for every non-empty selector field, in a
// fixed order.
func (s ErasureSelector) kinds() [][2]any {
	var out [][2]any
	for _, f := range []struct {
		kind string
		ids  []string
	}{
		{ErasureTargetDocument, s.DocumentIDs},
		{ErasureTargetThread, s.ThreadIDs},
		{ErasureTargetChunk, s.ChunkIDs},
		{ErasureTargetExperience, s.ExperienceIDs},
		{ErasureTargetEntity, s.EntityRefs},
		{ErasureTargetMemoryNote, s.MemoryNoteIDs},
	} {
		if len(f.ids) > 0 {
			out = append(out, [2]any{f.kind, SortedUniqueIDs(f.ids)})
		}
	}
	return out
}

// Kinds lists the selector kinds this selector names, in a fixed order.
func (s ErasureSelector) Kinds() []string {
	var out []string
	for _, k := range s.kinds() {
		out = append(out, k[0].(string))
	}
	return out
}

// IsEmpty reports whether the selector names nothing.
func (s ErasureSelector) IsEmpty() bool { return len(s.kinds()) == 0 }

// Count is the number of distinct ids the selector names.
func (s ErasureSelector) Count() int {
	n := 0
	for _, k := range s.kinds() {
		n += len(k[1].([]string))
	}
	return n
}

// Normalized returns the selector with every list sorted and de-duplicated (nil when
// empty), the form a store resolves and an id is computed from.
func (s ErasureSelector) Normalized() ErasureSelector {
	norm := func(ids []string) []string {
		if len(ids) == 0 {
			return nil
		}
		return SortedUniqueIDs(ids)
	}
	return ErasureSelector{
		DocumentIDs:   norm(s.DocumentIDs),
		ThreadIDs:     norm(s.ThreadIDs),
		ChunkIDs:      norm(s.ChunkIDs),
		ExperienceIDs: norm(s.ExperienceIDs),
		EntityRefs:    norm(s.EntityRefs),
		MemoryNoteIDs: norm(s.MemoryNoteIDs),
	}
}

// SortedUniqueIDs returns ids sorted and de-duplicated (a new slice).
func SortedUniqueIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	w := 0
	for i, id := range out {
		if i > 0 && id == out[w-1] {
			continue
		}
		out[w] = id
		w++
	}
	return out[:w]
}

// ErasureRequest is one forget: who asks, why, and what.
type ErasureRequest struct {
	Principal     string
	PrincipalKind ErasurePrincipalKind
	Reason        ErasureReason
	Selector      ErasureSelector
}

// Validate checks the request is well-formed (not whether it is authorized — that is S3).
func (r ErasureRequest) Validate() error {
	if strings.TrimSpace(r.Principal) == "" {
		return fmt.Errorf("%w: principal is required", ErrInvalidErasureRequest)
	}
	switch r.PrincipalKind {
	case ErasureByAgent, ErasureByOperator, ErasureByKernel:
	default:
		return fmt.Errorf("%w: unknown principal kind %q", ErrInvalidErasureRequest, r.PrincipalKind)
	}
	switch r.Reason {
	case ErasureReasonUserRequest, ErasureReasonCompliance, ErasureReasonCorrection:
	default:
		return fmt.Errorf("%w: unknown reason %q", ErrInvalidErasureRequest, r.Reason)
	}
	if r.Selector.IsEmpty() {
		return fmt.Errorf("%w: the selector names nothing (erasing everything requires naming it)", ErrInvalidErasureRequest)
	}
	for _, k := range r.Selector.kinds() {
		for _, id := range k[1].([]string) {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("%w: empty %s id", ErrInvalidErasureRequest, k[0])
			}
		}
	}
	return nil
}

// ID is the request's deterministic identity (ADR-0137 D6, AGENTS.md §2.6):
// "erase:" + the first 32 hex digits of sha256 over the principal (kind and id) and each
// named selector kind with its sorted, de-duplicated ids. No clock: a retry of the same
// request has the same id, and a store returns the receipt it already wrote. The reason
// is not part of the identity — the same act cannot be erased twice for two reasons.
func (r ErasureRequest) ID() string {
	h := sha256.New()
	// \x1e between fields and \x1f between ids: neither occurs in an id, so two
	// different requests cannot serialize to the same bytes.
	fmt.Fprintf(h, "%s:%s", r.PrincipalKind, r.Principal)
	for _, k := range r.Selector.kinds() {
		fmt.Fprintf(h, "\x1e%s=%s", k[0], strings.Join(k[1].([]string), "\x1f"))
	}
	return "erase:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// ErasureReceipt is the content-free record of one erasure (ADR-0137 D1, D8): ids,
// counts and times only.
type ErasureReceipt struct {
	RequestID     string
	Principal     string
	PrincipalKind ErasurePrincipalKind
	Reason        ErasureReason
	SelectorKinds []string
	// Status is ErasureStatusComplete or ErasureStatusPendingAsync.
	Status string
	// ErasedAt is the erase transaction's time — the no-resurrection fence (D5).
	ErasedAt time.Time
	// AsyncDeadline is when the outbox steps must be done (zero until the worker's
	// configuration sets it, S5).
	AsyncDeadline time.Time
	// Counts is rows deleted per store (table name → n).
	Counts map[string]int
	// Targets is the number of tombstone rows (erased units) recorded.
	Targets int
	// PendingSteps is the number of outbox steps written for the async tail.
	PendingSteps int
	// NotErasableExternal names the async steps (sorted) that ended without erasing
	// because their records live in an external service the kernel cannot delete from
	// (ErrErasureNotErasableExternal, ADR-0137 D7). Read from the outbox; empty until
	// such a step ran.
	NotErasableExternal []string
	// RedactedMessages are the ids (sorted) of the agent replies whose text the erase
	// replaced with RedactedReplyText because they rested on an erased record
	// (ENG-2109). Read from the tombstones; empty when no reply cited what went.
	RedactedMessages []string
	// Replayed is true when this receipt was returned for a retry of a request that had
	// already been executed: nothing was deleted by this call.
	Replayed bool
}

// MemoryEraser executes erasures. The Postgres implementation runs one transaction per
// request (ADR-0137 D4). It is the RAW eraser: it enforces nothing about who may erase
// what — callers reach it only through the enforcing decorator (authz.EnforcingEraser).
type MemoryEraser interface {
	// Erase executes req. Idempotent: a request whose ID already has a receipt returns
	// that receipt with Replayed=true and deletes nothing. Errors: ErrInvalidErasureRequest,
	// ErrNothingToErase (possibly as *NothingToEraseError), or a storage error, in which
	// case nothing was erased (the transaction rolled back).
	Erase(ctx context.Context, req ErasureRequest) (ErasureReceipt, error)
	// Receipt returns the stored receipt of requestID (found=false when there is none).
	Receipt(ctx context.Context, requestID string) (receipt ErasureReceipt, found bool, err error)
	// Resolve lists the stored records sel names (content-free candidates: ids, the
	// writer of record, tags), the basis of the authorization decision and of a preview.
	// Ids that match nothing are absent from the result, never an error.
	Resolve(ctx context.Context, sel ErasureSelector) ([]ErasureCandidate, error)
	// ListReceipts lists stored receipts newest first (ADR-0137 D8: an operator lists
	// all, a principal its own — the enforcing layer forces the filter).
	ListReceipts(ctx context.Context, f ErasureReceiptFilter) ([]ErasureReceipt, error)
}
