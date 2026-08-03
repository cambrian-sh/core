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
}

// NewIngestor builds an evidence ingestor. Both stores are required at
// construction (post-construction safety: a nil store must be a wiring error
// at boot, not a nil-pointer surprise on the first delivery).
func NewIngestor(blobs domain.ContentStore, store domain.EvidenceStore) (*Ingestor, error) {
	if blobs == nil || store == nil {
		return nil, fmt.Errorf("evidence ingestor: both a content store and an evidence store are required")
	}
	return &Ingestor{blobs: blobs, store: store}, nil
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
		Cursor:         raw.Cursor,
		TraceID:        raw.TraceID,
		RevisesID:      raw.RevisesID,
	})
	if err != nil {
		// The blob may now be an orphan. That is the deliberate trade (harmless
		// garbage over a dangling reference); GC owns the cleanup.
		return "", false, fmt.Errorf("evidence ingest: commit: %w", err)
	}
	return id, inserted, nil
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
