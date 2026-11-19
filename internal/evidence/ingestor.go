// Package evidence implements the write path of the knowledge substrate's
// evidence foundation (ADR-0105): preserve what arrived, immutably, before any
// semantic processing touches it.
//
// The package owns exactly one hard thing — the content-first commit ordering
// (memo §10) — and encodes it in control flow rather than convention:
//
//  1. bytes  → ContentStore.Put   (idempotent under the content hash)
//  2. verify → ContentStore.Has   (the pointer must resolve BEFORE it is published)
//  3. commit → EvidenceStore.Insert (evidence + outbox, atomically)
//  4. ONLY THEN may the caller acknowledge the sender
//  5. orphan blobs from crashes between 1 and 3 are garbage, not damage —
//     ContentStore.GC collects them
//
// The inverted order (rows before bytes) was a reviewed-and-caught bug: a crash
// between the two leaves an evidence row whose source material cannot be
// reprocessed — a silent, permanent hole in the archive.
package evidence

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// Raw is the delivery shape this package archives. It lives in domain
// (domain.RawEvidence) so plugins can hand deliveries to the kernel seam
// without importing internals; the alias keeps this package's vocabulary.
type Raw = domain.RawEvidence

// Ingestor turns raw deliveries into durable evidence under the ordering
// contract above.
type Ingestor struct {
	blobs domain.ContentStore
	store domain.EvidenceStore
	// fence is the store's early erasure answer when it offers one (ADR-0137 D5):
	// checked before the bytes are put, so a refused replay does not write erased
	// content back into the content store. nil for a store without one; the
	// authoritative fence is inside the store's Insert either way.
	fence domain.EvidenceErasureFence
}

// NewIngestor builds an evidence ingestor. Both stores are required at
// construction (post-construction safety: a nil store must be a wiring error
// at boot, not a nil-pointer surprise on the first delivery).
func NewIngestor(blobs domain.ContentStore, store domain.EvidenceStore) (*Ingestor, error) {
	if blobs == nil || store == nil {
		return nil, fmt.Errorf("evidence ingestor: both a content store and an evidence store are required")
	}
	fence, _ := store.(domain.EvidenceErasureFence)
	return &Ingestor{blobs: blobs, store: store, fence: fence}, nil
}

// contentNodeType labels evidence blobs in the ContentStore so GC policies and
// operators can tell an archive blob from workspace offload.
const contentNodeType = "evidence"

// snippetLimit bounds the inline resilience snippet stored beside the blob.
const snippetLimit = 256

// Ingest preserves one delivery and returns the evidence identity.
//
// inserted=false means the delivery was a replay of an already-archived
// (source, key, revision) triple: nothing was written anywhere, and the
// returned id is the existing row's. Either way, when Ingest returns nil the
// caller may acknowledge the sender — and MUST NOT before (ADR-0105 D2 step 4).
func (i *Ingestor) Ingest(ctx context.Context, raw Raw) (id domain.EvidenceID, inserted bool, err error) {
	if len(raw.Bytes) == 0 {
		return "", false, fmt.Errorf("evidence ingest: delivery carries no bytes")
	}
	if raw.SourceID == "" || raw.SourceKey == "" {
		return "", false, fmt.Errorf("evidence ingest: source_id and source_key are required")
	}

	// 0. ADR-0137 D5 (ENG-1866): a delivery whose source was erased after its
	// ingest origin (a reactive journal replay, a retried delivery) is refused
	// before anything is written — its bytes included, which an erase already
	// deleted from the content store. Insert re-checks under the erase lock.
	if i.fence != nil {
		erased, err := i.fence.SourceErased(ctx, raw.SourceKey)
		if err != nil {
			return "", false, fmt.Errorf("evidence ingest: erasure fence: %w", err)
		}
		if erased {
			return "", false, fmt.Errorf("evidence ingest: source %s: %w", raw.SourceKey, domain.ErrSourceErased)
		}
	}

	// 0b. ENG-1611: material a pipeline node fetched carries exactly its parent's
	// classification and parties (memo §12: derived records inherit
	// monotonically, never widen). Resolved before any byte is written.
	if raw.DerivedFrom != "" {
		if err := i.inheritFromParent(ctx, &raw); err != nil {
			return "", false, err
		}
	}

	// 1. Bytes first. Put is idempotent under the CID, so a crash-and-retry
	// re-puts the same content harmlessly.
	cid, err := i.blobs.Put(ctx, raw.Bytes, contentNodeType, nil, snippet(raw.Bytes))
	if err != nil {
		return "", false, fmt.Errorf("evidence ingest: store bytes: %w", err)
	}

	// 2. Verify the pointer resolves before anything references it. Publishing
	// evidence whose content is not retrievable is the exact failure the
	// ordering exists to prevent, so a false here is an error, not a warning.
	ok, err := i.blobs.Has(ctx, cid)
	if err != nil {
		return "", false, fmt.Errorf("evidence ingest: verify content %s: %w", cid, err)
	}
	if !ok {
		return "", false, fmt.Errorf("evidence ingest: content %s not retrievable after put", cid)
	}

	// 3. Atomically publish evidence + outbox, idempotent by source revision key.
	id, inserted, err = i.store.Insert(ctx, domain.Evidence{
		NamespaceID:    raw.NamespaceID,
		SourceID:       raw.SourceID,
		SourceKey:      raw.SourceKey,
		SourceRevision: raw.SourceRevision,
		SourceTime:     raw.SourceTime,
		ContentHash:    cid,
		ContentBytes:   int64(len(raw.Bytes)),
		Classification: raw.Classification,
		// Parties travels with Classification and for the same reason: both are
		// derived at the ingress, and both are meaningless if they do not reach
		// the row the policy filters. Omitting it here dropped every derived
		// party silently — the rules resolved, the delivery carried them, the
		// column existed and the INSERT bound it, and `evidence.parties` was
		// empty on every row in the deployment (ADR-0121).
		Parties:     raw.Parties,
		Cursor:      raw.Cursor,
		TraceID:     raw.TraceID,
		RevisesID:   raw.RevisesID,
		DerivedFrom: raw.DerivedFrom,
	})
	if err != nil {
		// The blob may now be an orphan. That is the deliberate trade (harmless
		// garbage over a dangling reference); GC owns the cleanup.
		return "", false, fmt.Errorf("evidence ingest: commit: %w", err)
	}
	return id, inserted, nil
}

// inheritFromParent gives a derived delivery its parent's classification and
// parties (ENG-1611). The parent must exist in the same namespace; the delivery
// may carry no labels of its own or exactly the parent's. Anything else is
// refused: a union could widen what a tag predicate admits, and a narrower set
// would widen it outright.
func (i *Ingestor) inheritFromParent(ctx context.Context, raw *Raw) error {
	parent, err := i.store.Get(ctx, raw.DerivedFrom)
	if err != nil || parent == nil {
		return fmt.Errorf("evidence ingest: %w: parent %s is not readable (%v); nothing derived from it is archived",
			domain.ErrDerivedEvidence, raw.DerivedFrom, err)
	}
	if orNS(parent.NamespaceID) != orNS(raw.NamespaceID) {
		return fmt.Errorf("evidence ingest: %w: parent %s is in namespace %q, the delivery in %q",
			domain.ErrDerivedEvidence, raw.DerivedFrom, parent.NamespaceID, raw.NamespaceID)
	}
	if len(raw.Classification) > 0 && !sameSet(raw.Classification, parent.Classification) {
		return fmt.Errorf("evidence ingest: %w: classification %v differs from the parent's %v",
			domain.ErrDerivedEvidence, raw.Classification, parent.Classification)
	}
	if len(raw.Parties) > 0 && !sameSet(raw.Parties, parent.Parties) {
		return fmt.Errorf("evidence ingest: %w: parties differ from the parent's", domain.ErrDerivedEvidence)
	}
	raw.Classification = append([]string(nil), parent.Classification...)
	raw.Parties = append([]string(nil), parent.Parties...)
	return nil
}

// orNS treats an empty namespace as the default one, as the stores do.
func orNS(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// sameSet reports whether a and b hold the same strings (order and duplicates
// ignored).
func sameSet(a, b []string) bool {
	in := func(xs []string) map[string]bool {
		m := make(map[string]bool, len(xs))
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	if len(ma) != len(mb) {
		return false
	}
	for k := range ma {
		if !mb[k] {
			return false
		}
	}
	return true
}

// Stage makes one delivery's bytes durable in the content store WITHOUT
// writing an evidence row — step 1 of the ordering contract, alone.
//
// Exists for the raw-delivery lane (ADR-0112 §6): a transport stages the
// original bytes, sends only the CID through the signal journal (bodies never
// ride the journal, ADR-0104), and the ingest_raw action later re-presents the
// bytes to Ingest — whose own Put is idempotent under the same CID. A crash
// between Stage and Ingest leaves an orphan blob: garbage, not damage, GC's
// problem. Defined HERE so the content-node shape (type, snippet) can never
// drift from what Ingest writes.
func (i *Ingestor) Stage(ctx context.Context, data []byte) (domain.CID, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("evidence stage: delivery carries no bytes")
	}
	cid, err := i.blobs.Put(ctx, data, contentNodeType, nil, snippet(data))
	if err != nil {
		return "", fmt.Errorf("evidence stage: store bytes: %w", err)
	}
	ok, err := i.blobs.Has(ctx, cid)
	if err != nil {
		return "", fmt.Errorf("evidence stage: verify content %s: %w", cid, err)
	}
	if !ok {
		return "", fmt.Errorf("evidence stage: content %s not retrievable after put", cid)
	}
	return cid, nil
}

// FetchStaged resolves staged bytes by CID — the read half the ingest_raw
// action needs to turn a journaled reference back into the original delivery.
func (i *Ingestor) FetchStaged(ctx context.Context, cid domain.CID) ([]byte, error) {
	node, err := i.blobs.Get(ctx, cid)
	if err != nil {
		return nil, fmt.Errorf("evidence fetch: content %s: %w", cid, err)
	}
	return node.Data, nil
}

// snippet returns a bounded, valid-UTF-8 prefix for the blob's inline snippet,
// or "" for binary content — mirroring the ContentStore's own convention.
func snippet(b []byte) string {
	if !utf8.Valid(b) {
		return ""
	}
	if len(b) <= snippetLimit {
		return string(b)
	}
	cut := b[:snippetLimit]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut)
}
