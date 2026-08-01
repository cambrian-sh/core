package app

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// kernelMemoryIngestor is the kernel's ReactiveMemoryIngestor (ADR-0104 D3).
//
// It runs a plugin's document through the SAME ingestion pipeline the agent plane
// and the operator plane use — chunking, structure parsing, entity extraction,
// embedding — rather than a shortcut beside it.
//
// That sameness is the entire point. The drift lane previously detected over text
// it then dropped, while memory was populated by a SEPARATE call the caller had to
// remember to make. A connector that called only the drift RPC produced alerts over
// an empty brain: nothing retrievable, no chunks, no extracted entities. Two write
// paths is two shapes of memory depending on which door content arrived through —
// the same defect the operator plane's ingest comment already records, one lane
// over.
type kernelMemoryIngestor struct {
	processor interface {
		ProcessSync(ctx context.Context, doc domain.ExternalDocument) (string, error)
	}
	// principal is stamped on every write. Without it the write reaches the
	// classification chokepoint with no identity: OSS fails open and never notices,
	// while a premium deployment fails CLOSED with `no_principal`. That asymmetry is
	// exactly why an unstamped path can survive for months — it only breaks where
	// the check actually works.
	principal domain.PrincipalRef
}

// Ingest runs one document through the standard pipeline.
func (k *kernelMemoryIngestor) Ingest(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	if k == nil || k.processor == nil {
		// No silent fallback to a raw store write. That produced a structurally
		// different row — un-chunked, no source-document entity, invisible to
		// ListDocuments — so the same action wrote two shapes of memory depending on
		// configuration. Failing is the honest outcome.
		return "", fmt.Errorf("ingestion pipeline not configured: a lane that receives " +
			"content cannot store it, and detecting over content nobody stored would " +
			"produce alerts nothing can corroborate")
	}
	if doc.Body == "" && len(doc.Data) == 0 {
		return "", fmt.Errorf("ingest: document carries no body")
	}
	return k.processor.ProcessSync(domain.WithPrincipal(ctx, k.principal), doc)
}
