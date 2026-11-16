package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ADR-0137 S6–S7 (ENG-411 stage C): what the forget contract (the agent- and
// operator-plane RPCs, the forget_memory tool) and the plugin seams around the erase
// transaction need from the domain.

// ErrErasureDisabled: no eraser is composed (execution.erasure.enabled is off, or the
// memory backend has none). The RPC boundary answers Unimplemented with this text, so a
// caller can tell "this kernel cannot erase" from "you may not" (ErrEraseDenied) and
// "nothing matched" (ErrNothingToErase).
var ErrErasureDisabled = errors.New("memory erasure is disabled on this kernel (execution.erasure.enabled)")

// ErrPreviewUnknown: Forget named a preview token this kernel did not issue to this
// caller, or whose preview expired (execution.erasure.preview_ttl_seconds). The caller
// previews again; a token is never a capability (ADR-0137 D2).
var ErrPreviewUnknown = errors.New("unknown or expired erasure preview")

// ErasureReceiptFilter narrows a receipt listing. Zero fields match all.
type ErasureReceiptFilter struct {
	// Principal keeps only receipts whose principal (the WriterID of whoever asked)
	// is this. The enforcing eraser forces it to the caller for an agent principal.
	Principal string
	// Before is a cursor: only receipts of requests erased strictly before the receipt
	// with this request id (newest first). "" starts at the newest.
	Before string
	// Limit bounds the page; 0 ⇒ 50, capped at 500 by the store.
	Limit int
}

// ErasureTx is the slice of the erase transaction a participant may use: statements
// that run INSIDE the one Postgres transaction of ADR-0137 D4 and commit or roll back
// with it. Deliberately not a pgx type, so domain/ and plugins stay driver-free.
type ErasureTx interface {
	// Exec runs one statement and returns the number of rows it affected.
	Exec(ctx context.Context, sql string, args ...any) (rowsAffected int64, err error)
}

// ErasureParticipant erases a plugin's own rows that derive from (or reference) the
// ADR-0105 evidence an erasure removes — premium ingress-studio deliveries, projections
// and transform runs (ADR-0137 D4 step 8, inventory row 27). It runs inside the erase
// transaction BEFORE the evidence rows are deleted, so a row that still references the
// evidence never makes the whole erase roll back. Add-many (app.Registry).
//
// A participant must delete only rows keyed by the evidence ids it is handed, return
// content-free counts per table, and fail the erase (return an error) rather than leave
// a row it should have removed: the transaction then rolls back and nothing is erased.
type ErasureParticipant interface {
	// Name identifies the participant in errors and logs.
	Name() string
	// EraseEvidence deletes the participant's rows for evidenceIDs (sorted, non-empty)
	// through tx and returns rows deleted per table (merged into the receipt's counts).
	EraseEvidence(ctx context.Context, tx ErasureTx, evidenceIDs []string) (map[string]int, error)
}

// ErasureObserver receives every executed erasure's content-free receipt after its
// transaction committed — the seam of the premium signed ADR-0103 erasure receipt
// (ADR-0137 D8, Q7). Not called for a replayed request (nothing was erased by it).
// Add-many. MUST return promptly and MUST NOT panic: it runs on the erase call path.
type ErasureObserver interface {
	ObserveErasure(ctx context.Context, rec ErasureReceipt)
}

// MultiErasureObserver fans one receipt out to several observers, in order.
type MultiErasureObserver []ErasureObserver

// ObserveErasure implements ErasureObserver.
func (m MultiErasureObserver) ObserveErasure(ctx context.Context, rec ErasureReceipt) {
	for _, o := range m {
		if o != nil {
			o.ObserveErasure(ctx, rec)
		}
	}
}

// ErasurePreviewToken is the deterministic token of a forget preview (ADR-0137 D2):
// "pv:" + the first 32 hex digits of sha256 over the asking principal (PrincipalRef.String,
// kind and id), the
// concrete normalized selector the preview resolved to, and the fingerprint of the
// erasure policy in force. No clock: the same preview of the same records under the same
// policy yields the same token, and a policy change invalidates every outstanding one.
func ErasurePreviewToken(principal string, sel ErasureSelector, policyFingerprint string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x1e%s", principal, policyFingerprint)
	for _, k := range sel.Normalized().kinds() {
		fmt.Fprintf(h, "\x1e%s=%s", k[0], strings.Join(k[1].([]string), "\x1f"))
	}
	return "pv:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// SelectorOfCandidates is the concrete selector a set of resolved candidates names:
// documents (also those a thread resolved to), chunks, experiences and entities by id.
// A preview executes exactly this, so records that appear on a thread after the preview
// are never erased by it.
func SelectorOfCandidates(cands []ErasureCandidate) ErasureSelector {
	var s ErasureSelector
	for _, c := range cands {
		switch c.Kind {
		case ErasureTargetDocument:
			s.DocumentIDs = append(s.DocumentIDs, c.ID)
		case ErasureTargetChunk:
			s.ChunkIDs = append(s.ChunkIDs, c.ID)
		case ErasureTargetExperience:
			s.ExperienceIDs = append(s.ExperienceIDs, c.ID)
		case ErasureTargetEntity:
			s.EntityRefs = append(s.EntityRefs, c.ID)
		}
	}
	return s.Normalized()
}

// ParseErasureReason maps a wire reason code to the closed set; "" ⇒ user_request.
func ParseErasureReason(code string) (ErasureReason, error) {
	switch ErasureReason(strings.TrimSpace(code)) {
	case "", ErasureReasonUserRequest:
		return ErasureReasonUserRequest, nil
	case ErasureReasonCompliance:
		return ErasureReasonCompliance, nil
	case ErasureReasonCorrection:
		return ErasureReasonCorrection, nil
	}
	return "", fmt.Errorf("%w: unknown reason code %q (user_request, compliance, correction)", ErrInvalidErasureRequest, code)
}
