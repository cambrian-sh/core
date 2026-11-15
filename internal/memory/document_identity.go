package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

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
	// Upload is true for an operator upload keyed by its name (ADR-0138 B1.2, rule U):
	// lineage-keyed by (writer, classification scope, normalised file name). Only such
	// an ingest is deduplicated by content (B1.1) and matched by name (B1.3).
	Upload bool
	// UploadScope is the upload's classification scope (uploadScope); nil otherwise.
	UploadScope []string
}

// uploadIDScheme prefixes a rule-U id. It is the scheme the operator door already
// gives an upload's SourceURI, so an upload with an empty scope and an already-normal
// name keeps exactly the rule-3 id it had before B1 (no re-key, no lost lineage).
const uploadIDScheme = "operator_ingest://"

// normaliseUploadName is the B1.2 name key (decision logged on
// T-20261104-operator-upload-cannot-replace-a-document-a-re): the last path segment
// (a client may send `C:\x\report.pdf` or "dir/report.pdf"), Unicode NFC (macOS
// file systems hand out NFD), surrounding whitespace trimmed, internal whitespace runs
// collapsed to one space, and lower-cased — so "Report.PDF" and " report.pdf " are one
// document, and "report (2).pdf" is another.
func normaliseUploadName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = norm.NFC.String(name)
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// isMarkerTag reports the tags that carry no classification: the source_document
// marker and chunker hints. Rule 1b skips the same set.
//
// It used to skip "document-qa" too, the tag of one benchmark suite — benchmark logic in
// the OSS kernel (decisions/no-benchmark-in-oss.md; ENG-1195). That suite always names its
// document with `source_document <id>` (rule 1a, which wins before this is consulted), so
// the tag is now an ordinary classification tag like any other. legacyExternalDocumentID
// keeps the old skip byte for byte: it reproduces pre-ADR-0138 ids for grandfathering.
func isMarkerTag(tag string) bool {
	return tag == "" || tag == sourceDocumentMarker || strings.HasPrefix(tag, "chunker:")
}

// uploadScope is the B1.2 classification scope of an upload: its request tags minus
// the marker tags, de-duplicated and sorted. Case-sensitive, because access policy
// matches tags exactly. The session is deliberately NOT part of it: replacing and
// deduplicating across sessions is the point of B1.
func uploadScope(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if isMarkerTag(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// uploadID is the rule-U id: the scheme, the normalised name, the scope (16 hex of the
// sha256 of its NUL-joined tags; omitted for the empty scope) and the writer suffix.
// Ids stay bounded and carry no principal or label names.
func uploadID(name string, scope []string, writer string) string {
	id := uploadIDScheme + normaliseUploadName(name)
	if len(scope) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(scope, "\x00")))
		id += "#s=" + hex.EncodeToString(sum[:8])
	}
	return id + writerSuffix(writer)
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
//     U. an operator upload (UploadName, a stamped writer): named by (writer,
//     classification scope, normalised file name) — Amendment B1.2. Another writer's
//     upload of the same name is another id, so it is never touched.
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
// Rules 1, U and 3 are lineage-keyed (a changed re-ingest is a new VERSION of the same id,
// ADR-0138 D3); 1b, 2 and 4 are content-addressed (a changed content is another item).
func documentIdentityOf(doc domain.ExternalDocument) docIdentity {
	id := docIdentity{Writer: doc.Author, LegacyID: legacyExternalDocumentID(doc), ContentDigest: revisionDigest(doc)}
	for i, tag := range doc.Tags {
		if tag == sourceDocumentMarker && i+1 < len(doc.Tags) && doc.Tags[i+1] != "" {
			id.ID = doc.Tags[i+1]
			return id
		}
	}
	if doc.UploadName != "" && doc.Author != "" && normaliseUploadName(doc.UploadName) != "" {
		id.Upload, id.UploadScope = true, uploadScope(doc.Tags)
		id.ID = uploadID(doc.UploadName, id.UploadScope, doc.Author)
		return id
	}
	for _, tag := range doc.Tags {
		if isMarkerTag(tag) {
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
		// Amendment B1 is off under the legacy scheme: no upload key, no dedup, no
		// association — the old behaviour exactly.
		id.Upload, id.UploadScope = false, nil
		return id
	}
	id := documentIdentityOf(doc)
	if id.Upload {
		return im.resolveUploadID(ctx, doc, id)
	}
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

// resolveUploadID is resolveDocumentID for a rule-U upload (ADR-0138 B1.2). Before B1 an
// upload was keyed by the other rules; the earlier key is grandfathered only when it
// was LINEAGE-keyed (a named SourceURI: no tag, no session) and the stored row is this
// writer's, so the next changed upload of that file versions the stored document
// instead of starting a second one. A content-addressed earlier key (tag or session)
// is never reused: it names one revision, not the file — an identical re-upload of it
// is found by content instead (B1.1).
func (im *IngestionManager) resolveUploadID(ctx context.Context, doc domain.ExternalDocument, id docIdentity) docIdentity {
	if im.lifecycle == nil {
		return id
	}
	cur, err := im.lifecycle.DocumentRecord(ctx, id.ID)
	if err != nil {
		slog.WarnContext(ctx, "IngestionManager: upload identity lookup failed; using the upload id",
			"doc", id.ID, "err", err)
		return id
	}
	if cur.Exists {
		return id
	}
	before := doc
	before.UploadName = ""
	prior := documentIdentityOf(before)
	var candidates []string
	if !prior.ContentAddressed && prior.ID != id.ID {
		candidates = append(candidates, prior.ID) // the ADR-0138 rule-3 id
	}
	if !prior.ContentAddressed && id.LegacyID != prior.ID && id.LegacyID != id.ID {
		candidates = append(candidates, id.LegacyID) // the pre-ADR-0138 rule-3 id
	}
	for _, c := range candidates {
		old, err := im.lifecycle.DocumentRecord(ctx, c)
		if err != nil {
			slog.WarnContext(ctx, "IngestionManager: prior upload identity lookup failed; using the upload id",
				"doc", id.ID, "prior", c, "err", err)
			return id
		}
		if old.Exists && old.WrittenBy == id.Writer {
			id.ID = c // grandfathered (ADR-0138 D2, applied to B1.2)
			return id
		}
	}
	return id
}

// uploadDuplicate is ADR-0138 B1.1: an operator upload whose exact revision this writer
// already stores is a no-op. It returns the stored document (Exists=true) when the
// ingest must write nothing. Only rule-U uploads are deduplicated; a lookup failure
// logs and lets the ingest proceed (it then writes, which is the pre-B1 behaviour).
func (im *IngestionManager) uploadDuplicate(ctx context.Context, ident docIdentity) domain.DocumentRecord {
	if !ident.Upload || im.lifecycle == nil || im.cfg.LegacyDocumentIdentity {
		return domain.DocumentRecord{}
	}
	rec, err := im.lifecycle.FindByContent(ctx, ident.Writer, ident.ContentDigest)
	if err != nil {
		slog.WarnContext(ctx, "IngestionManager: duplicate-upload lookup failed; ingesting",
			"doc", ident.ID, "err", err)
		return domain.DocumentRecord{}
	}
	return rec
}

// recordUpload writes the B1.1 association of an upload with its session and scope,
// best-effort: the row is bookkeeping, and failing an ingest whose content is already
// stored because its bookkeeping could not be written would be the worse outcome.
func (im *IngestionManager) recordUpload(ctx context.Context, doc domain.ExternalDocument, ident docIdentity, documentID string) {
	if !ident.Upload || im.lifecycle == nil {
		return
	}
	if err := im.lifecycle.RecordAssociation(ctx, domain.DocumentAssociation{
		DocumentID: documentID, Writer: ident.Writer, SessionID: doc.ThreadID,
		Scope: ident.UploadScope, UploadName: doc.UploadName,
	}); err != nil {
		slog.WarnContext(ctx, "IngestionManager: upload association not recorded", "doc", documentID, "err", err)
	}
}

// MatchUpload reports the stored document an operator upload of doc would REPLACE
// (ADR-0138 B1.3): the console asks "Replace <name>?" only when this finds one. It
// resolves the id with exactly the code the ingest uses (resolveDocumentID), so the
// prompt and the write cannot disagree. Exists=false when doc is not a rule-U upload,
// under the legacy identity scheme, or when nothing is stored under the id. The record
// is the caller's own document (the id is writer-qualified and the writer of record is
// checked), so the lookup reveals nothing another principal wrote.
func (im *IngestionManager) MatchUpload(ctx context.Context, doc domain.ExternalDocument) (domain.DocumentRecord, error) {
	if im.cfg.LegacyDocumentIdentity || im.lifecycle == nil {
		return domain.DocumentRecord{}, nil
	}
	ident := im.resolveDocumentID(ctx, doc)
	if !ident.Upload {
		return domain.DocumentRecord{}, nil
	}
	rec, err := im.lifecycle.DocumentRecord(ctx, ident.ID)
	if err != nil {
		return domain.DocumentRecord{}, err
	}
	if !rec.Exists || rec.WrittenBy != ident.Writer {
		return domain.DocumentRecord{}, nil
	}
	return rec, nil
}

// UploadReplaceEnabled reports whether Amendment B1 is in force: document identity v2
// with a lifecycle store. The composition root gates the console's replace prompt
// (capability "upload-replace") on it.
func (im *IngestionManager) UploadReplaceEnabled() bool {
	return im != nil && !im.cfg.LegacyDocumentIdentity && im.lifecycle != nil
}

// SetDocumentLifecycle wires the ADR-0138 document lifecycle (identity lookup, writer
// check, version retirement). Called by kernel.NewMemoryStack, the one composition
// point, right after construction; nil is ignored.
func (im *IngestionManager) SetDocumentLifecycle(l domain.DocumentLifecycleStore) {
	if im != nil && l != nil {
		im.lifecycle = l
	}
}
