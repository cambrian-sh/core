package app

import (
	"context"
	"fmt"
	"log/slog"

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
//
// # Why the decision point is a constructor argument
//
// EnforcingVectorStore's by-id reads take their predicate from ctx ONLY
// (authz.EnforcingVectorStore.readPredicate); a principal on ctx is not a
// predicate, and nothing between this reader and the decorator resolves one. Until
// T-20261114-kerneldocumentreader-seeds-only-the-principal this reader stamped only
// the principal, so every non-system read (MCP get_document, plugin DocReader)
// failed closed with authz.ErrScopeMissing. The predicate is therefore resolved HERE
// from the kernel's effective Authorizer, held from construction (AGENTS.md §2.3) —
// never authz.AuthorizerFromContext, which a plugin context may not carry and whose
// fallback would be allow-all. This is the same resolution the knowledge-query seam
// does (authz.QueryKnowledgeFunc, ADR-0118 D1).
type kernelDocumentReader struct {
	// store is the ENFORCING store. Handing this the raw adapter would skip the
	// decorator that is the enforcement point.
	store domain.VectorReader
	// authorizer is the decision point that turns a principal into its read
	// predicate. nil refuses every non-system read rather than allowing it.
	authorizer domain.Authorizer
}

// newKernelDocumentReader builds the reader over the tier-1 enforcing read store
// and the kernel's resolved Authorizer (ADR-0085).
func newKernelDocumentReader(store domain.VectorReader, authorizer domain.Authorizer) *kernelDocumentReader {
	return &kernelDocumentReader{store: store, authorizer: authorizer}
}

// GetDocument resolves a reference for one principal.
//
// The principal's read predicate is resolved by the decision point and stamped on
// the context, the only channel the decorator's by-id reads consult (AGENTS.md
// §2.4). A principal granted no read gets ErrDocumentNotFound — the same answer as
// an absent or filtered row, so a denial cannot confirm the document exists.
func (r *kernelDocumentReader) GetDocument(ctx context.Context, principal domain.PrincipalRef, id string) (domain.Document, error) {
	if r.store == nil {
		return domain.Document{}, fmt.Errorf("document reads are not configured in this deployment")
	}
	if id == "" {
		return domain.Document{}, fmt.Errorf("get document: empty id")
	}

	scoped, ok := r.withPrincipalScope(ctx, principal)
	if !ok {
		return domain.Document{}, domain.ErrDocumentNotFound
	}
	doc, err := r.store.GetByID(scoped, id)
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

// withPrincipalScope stamps the read predicate the enforcing store applies, plus
// the principal itself for anything downstream that reports on the caller.
//
// The system principal maps to ScopeSystem, which is what an unadministered plugin
// reads as — forbid every closed tag, the same fallback the (since removed) premium
// record lane used — rather than inventing a second answer for the same question.
// Every other principal gets the predicate the Authorizer grants it on the
// transport surface already on ctx (the MCP endpoint stamps SurfaceMCP); false
// means no read is authorized.
func (r *kernelDocumentReader) withPrincipalScope(ctx context.Context, p domain.PrincipalRef) (context.Context, bool) {
	if p.Kind == domain.PrincipalSystem {
		return domain.WithScope(ctx, domain.ScopeSystem), true
	}
	if r.authorizer == nil {
		return ctx, false
	}
	pred, dec := r.authorizer.ReadFilter(ctx, p, domain.SurfaceFromContext(ctx))
	if pred == nil {
		slog.DebugContext(ctx, "document read denied: no read predicate",
			slog.String("principal", p.String()), slog.String("reason", string(dec.Reason)))
		return ctx, false
	}
	return domain.WithScope(domain.WithPrincipal(ctx, p), pred), true
}
