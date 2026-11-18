package domain

import (
	"context"
	"errors"
)

// ADR-0137 D5 / ADR-0138 D3 (ENG-501): the version half of the write fence.
//
// A changed document is revised in place: ReviseDocument retires the stored version and
// the new version reuses its chunk ids (<doc>-chunk-<n>). The KG triplet batcher and the
// fact extractor are asynchronous and keyed by chunk id, so a job queued by the PREVIOUS
// version's ingest can drain after the new chunks exist and would write the old text's
// triplets, facts and extraction marker onto the new version. The ingest therefore
// stamps the version it wrote — the document id and its content digest — on the context
// of those jobs, and the store refuses a write whose stamp no longer matches
// documents.content_digest, inside the same transaction as the write and under the same
// per-document lock as the erasure fence.

// ErrSourceVersionRetired is returned by a store write whose context names a document
// version (WithSourceVersion) that is no longer the document's current one: the document
// was revised after the work that produced the write began. Like ErrSourceErased it is
// final — drop, count, never requeue: the new version's own jobs produce its derivatives.
var ErrSourceVersionRetired = errors.New("source document version retired")

// SourceVersion names the document version a derived write was extracted from.
type SourceVersion struct {
	DocumentID    string
	ContentDigest string
}

type sourceVersionKey struct{}

// WithSourceVersion returns ctx carrying v. It is a context VALUE, so it survives
// context.WithoutCancel and rides an async job's continuation (AGENTS.md §2.7). A
// version with an empty document id or digest is ignored: there is nothing to compare.
func WithSourceVersion(ctx context.Context, v SourceVersion) context.Context {
	if v.DocumentID == "" || v.ContentDigest == "" {
		return ctx
	}
	return context.WithValue(ctx, sourceVersionKey{}, v)
}

// SourceVersionFrom returns the source version ctx carries.
func SourceVersionFrom(ctx context.Context) (SourceVersion, bool) {
	v, ok := ctx.Value(sourceVersionKey{}).(SourceVersion)
	return v, ok && v.DocumentID != "" && v.ContentDigest != ""
}
