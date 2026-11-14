package memory

import (
	"errors"

	"github.com/cambrian-sh/core/domain"
)

// ErrDocumentWriteDenied is returned when the decision point refuses to classify a
// document write. Deliberately coarse — the specific reason travels in the
// AccessDecision, which the chokepoint logs, so the error cannot be used to probe
// the vocabulary.
var ErrDocumentWriteDenied = errors.New("authz: document write denied by policy")

// DocumentFilter, DocumentSummary and the DocumentLister port live in
// domain/memory_ports.go (ENG-1104: the postgres adapter implements the port without
// importing this package); aliased here for existing callers.
type (
	DocumentFilter  = domain.DocumentFilter
	DocumentSummary = domain.DocumentSummary
	DocumentLister  = domain.DocumentLister
)

// SetDocumentStore wires the source-document entity onto the ingest path. Optional:
// a nil store means documents are not recorded and chunks keep a NULL parent, which
// is the pre-ADR-0093 behaviour rather than a failure.
func (im *IngestionManager) SetDocumentStore(store domain.DocumentStore) {
	if im != nil && store != nil {
		im.documentStore = store
	}
}

// ErrDocumentNotFound and DocumentGetter live in domain so the premium module can
// depend on them (internal/ is unimportable there). Aliased here because this
// package is where the other document ports live and a reader looking for them
// should find them, not a dead end.
var ErrDocumentNotFound = domain.ErrDocumentNotFound

// DocumentGetter is domain.DocumentGetter.
type DocumentGetter = domain.DocumentGetter
