package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// docIdentity is one ingest's resolved document identity (ADR-0138 D1).
type docIdentity struct {
	// ID is the v2 id (or the legacy id under the legacy scheme).
	ID string
	// LegacyID is what legacyExternalDocumentID gives for the same document: the id a
	// store written before ADR-0138 may already hold it under.
	LegacyID string
	// ContentAddressed is true for the rules whose id embeds the content digest (tag
	// group, thread, unnamed). Such an id never forms a version lineage, and for a binary
	// its legacy id is the one that collided, so it is never grandfathered.
	ContentAddressed bool
	// Writer is the kernel-stamped writer (ExternalDocument.Author).
	Writer string
	// ContentDigest is the full revision digest stored on the document (D1.3).
	ContentDigest string
}

// revisionBytes is the content a document's identity and version are computed over
// (ADR-0138 D1.3): Body for a text document; for a binary (bytes in Data) the length of
// Body, Body, then Data — so two PDFs whose Body is empty, or holds the same context
// note, never share a digest. ADR-0105 evidence capture hashes the same payload.
func revisionBytes(doc domain.ExternalDocument) []byte {
	if len(doc.Data) == 0 {
		return []byte(doc.Body)
	}
	var n [binary.MaxVarintLen64]byte
	k := binary.PutUvarint(n[:], uint64(len(doc.Body)))
	out := make([]byte, 0, k+len(doc.Body)+len(doc.Data))
	out = append(out, n[:k]...)
	out = append(out, doc.Body...)
	return append(out, doc.Data...)
}

// revisionDigest is the full hex sha256 of revisionBytes: documents.content_digest.
func revisionDigest(doc domain.ExternalDocument) string {
	sum := sha256.Sum256(revisionBytes(doc))
	return hex.EncodeToString(sum[:])
}

// contentKey is the content-addressed part of a v2 id: 16 hex of sha256 over the writer
// (when there is one) and revisionBytes. With no writer and a text body it equals the
// legacy contentDigest(Body), so writer-less text ingests keep their ids exactly.
func contentKey(writer string, doc domain.ExternalDocument) string {
	h := sha256.New()
	if writer != "" {
		h.Write([]byte(writer))
		h.Write([]byte{0})
	}
	h.Write(revisionBytes(doc))
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// writerSuffix qualifies a named-source (rule 3) id with its writer, as 16 hex of the
// writer's sha256 so ids stay bounded and carry no principal name.
func writerSuffix(writer string) string {
	sum := sha256.Sum256([]byte(writer))
	return "#w=" + hex.EncodeToString(sum[:8])
}

// namesSource reports whether a SourceURI names something. The ingest doors synthesise
// "<scheme>://" + session id; with no session that is the bare scheme, shared by every
// such ingest of every caller, so it identifies nothing (ADR-0138 observation).
func namesSource(uri string) bool {
	if uri == "" {
		return false
	}
	if i := strings.Index(uri, "://"); i >= 0 {
		return strings.Trim(uri[i+3:], "/") != ""
	}
	return true
}

// externalDocumentID is the v2 id of doc with no store lookup (documentIdentityOf).
func externalDocumentID(doc domain.ExternalDocument) string {
	return documentIdentityOf(doc).ID
}

// documentIdentityOf derives the ADR-0138 D1 v2 identity. The natural key is (writer,
// source key); the writer is ExternalDocument.Author, which every principal-serving door
// stamps from the authenticated caller and a client can never set (ADR-0136 A3.2). A
// writer-less ingest keeps the legacy id (except the two collision fixes, which apply to
// everyone: binaries digest Data, and a bare-scheme URI is not a source).
//
//  1. source_document marker: the caller-NAMED id, verbatim — a downstream contract
//     (metadata document_id / "<id>-chunk-N"). Another writer of a held name is refused
//     by the lifecycle, never qualified (Q1).
//
//     1b. a classification tag: it GROUPS, never names; the content key separates.
//
//  2. a thread: many distinct turns share it; the content key separates.
//
//  3. a SourceURI that names something, with no tag and no thread: the named source,
//     qualified by its writer.
//
//  4. nothing: the content key alone.
//
// Rules 1 and 3 are lineage-keyed (a changed re-ingest is a new VERSION of the same id,
// ADR-0138 D3); 1b, 2 and 4 are content-addressed (a changed content is another item).
func documentIdentityOf(doc domain.ExternalDocument) docIdentity {
	id := docIdentity{Writer: doc.Author, LegacyID: legacyExternalDocumentID(doc), ContentDigest: revisionDigest(doc)}
	for i, tag := range doc.Tags {
		if tag == sourceDocumentMarker && i+1 < len(doc.Tags) && doc.Tags[i+1] != "" {
			id.ID = doc.Tags[i+1]
			return id
		}
	}
	for _, tag := range doc.Tags {
		if tag == "" || tag == "document-qa" || tag == sourceDocumentMarker || strings.HasPrefix(tag, "chunker:") {
			continue
		}
		id.ID, id.ContentAddressed = tag+":"+contentKey(doc.Author, doc), true
		return id
	}
	if doc.ThreadID != "" {
		id.ID, id.ContentAddressed = doc.ThreadID+":"+contentKey(doc.Author, doc), true
		return id
	}
	if namesSource(doc.SourceURI) {
		id.ID = doc.SourceURI
		if doc.Author != "" {
			id.ID += writerSuffix(doc.Author)
		}
		return id
	}
	id.ID, id.ContentAddressed = "external-document:"+contentKey(doc.Author, doc), true
	return id
}

// resolveDocumentID resolves doc's id ONCE per ingest (ADR-0138 D1.4); evidence capture,
// the document row, the chunks, the fact job and the source-doc entity all use the
// result.
//
// Under the v2 scheme, when the v2 id differs from the legacy one (ADR-0138 D2):
//  1. a stored document under the v2 id → the v2 id;
//  2. else a stored document under the LEGACY id whose writer of record is this writer
//     (and, for a content-addressed id, no binary payload: a binary's legacy id is the
//     one that collided) → the legacy id, so re-ingesting stored content is idempotent
//     and never duplicated;
//  3. else the v2 id.
//
// A lookup failure logs and takes the v2 id: the id stays deterministic and the write
// that follows reports a store that is down.
func (im *IngestionManager) resolveDocumentID(ctx context.Context, doc domain.ExternalDocument) docIdentity {
	if im.cfg.LegacyDocumentIdentity {
		id := documentIdentityOf(doc)
		id.ID = id.LegacyID
		return id
	}
	id := documentIdentityOf(doc)
	if id.ID == id.LegacyID || im.lifecycle == nil {
		return id
	}
	if id.ContentAddressed && len(doc.Data) > 0 {
		return id
	}
	cur, err := im.lifecycle.DocumentRecord(ctx, id.ID)
	if err != nil {
		slog.WarnContext(ctx, "IngestionManager: document identity lookup failed; using the v2 id",
			"doc", id.ID, "err", err)
		return id
	}
	if cur.Exists {
		return id
	}
	old, err := im.lifecycle.DocumentRecord(ctx, id.LegacyID)
	if err != nil {
		slog.WarnContext(ctx, "IngestionManager: legacy document identity lookup failed; using the v2 id",
			"doc", id.ID, "legacy", id.LegacyID, "err", err)
		return id
	}
	if old.Exists && old.WrittenBy == id.Writer {
		id.ID = id.LegacyID // grandfathered (ADR-0138 D2)
	}
	return id
}

// SetDocumentLifecycle wires the ADR-0138 document lifecycle (identity lookup, writer
// check, version retirement). Called by kernel.NewMemoryStack, the one composition
// point, right after construction; nil is ignored.
func (im *IngestionManager) SetDocumentLifecycle(l domain.DocumentLifecycleStore) {
	if im != nil && l != nil {
		im.lifecycle = l
	}
}
