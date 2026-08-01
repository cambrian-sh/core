package app

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// kernelDocumentReader is the kernel's implementation of ReactiveDocumentReader
// (ADR-0104 D6.2): a plugin asks for a document by reference, and the KERNEL
// decides what that plugin's principal may see.
//
// # It delegates to the enforcing store rather than reading the database
//
// The first version of this file carried its own scoped SQL against `documents`.
// That was wrong twice over, and both mistakes are ones this codebase keeps making:
//
//   - `EnforcingVectorStore.GetByID` ALREADY does exactly this — scoped, and
//     reporting an unreadable row as absent so a denial cannot confirm existence.
//     Writing a second one was building a primitive that existed.
//   - The hand-written version read only `documents`, while by-id lookups span
//     `idLookupTables` (chunks first, which holds the overwhelming majority of rows
//     AND every `source_doc:` entity). It resolved almost nothing, and the live
//     message watch dead-lettered 60 times before that surfaced.
//
// A second copy of a scoped read is a second place for the access model to drift,
// and the copy that drifts is the one nobody is testing.
//
// # Why a principal, never a predicate
//
// A plugin writes to memory through KernelServices rather than touching a store;
// reading is symmetric. A seam accepting a `*TagPredicate` would let the plugin
// choose its own access scope — not an extension point, a bypass with extra steps.
// So the plugin says WHO it is, and the scope is resolved here.
type kernelDocumentReader struct {
	// store is the ENFORCING store. Handing this the raw adapter would skip the
	// decorator that is the enforcement point.
	store domain.VectorStore
}

// GetDocument resolves a reference for one principal.
//
// The principal is carried on the context as the read scope, which is how every
// other enforced read in the kernel identifies its caller — the decorator resolves
// the predicate from it.
func (r *kernelDocumentReader) GetDocument(ctx context.Context, principal domain.PrincipalRef, id string) (domain.Document, error) {
	if r.store == nil {
		return domain.Document{}, fmt.Errorf("document reads are not configured in this deployment")
	}
	if id == "" {
		return domain.Document{}, fmt.Errorf("get document: empty id")
	}

	doc, err := r.store.GetByID(withPrincipalScope(ctx, principal), id)
	if err != nil {
		return domain.Document{}, err
	}
	if doc == nil {
		// Absent, or present and unreadable by this principal — deliberately the
		// same answer. Distinguishing them would confirm the document exists, which
		// is the disclosure the predicate is withholding.
		return domain.Document{}, domain.ErrDocumentNotFound
	}
	return *doc, nil
}

// withPrincipalScope stamps the principal the enforcing store resolves against.
//
// The system principal maps to ScopeSystem, which is what an unadministered plugin
// reads as — matching the record lane's own fallback (`defaultScope`: forbid every
// closed tag) rather than inventing a second answer for the same question.
func withPrincipalScope(ctx context.Context, p domain.PrincipalRef) context.Context {
	if p.Kind == domain.PrincipalSystem {
		return domain.WithScope(ctx, domain.ScopeSystem)
	}
	return domain.WithPrincipal(ctx, p)
}
