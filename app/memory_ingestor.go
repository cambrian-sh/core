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
	// connector names who brought the content in. Every write is stamped with the
	// SOURCE service principal source:<connector>:<doc source> (ENG-494, owner
	// decision 2026-11-15). Without a principal the write reaches the classification
	// chokepoint with no identity: OSS fails open and never notices, while a premium
	// deployment fails CLOSED with `no_principal` — exactly why an unstamped path can
	// survive for months. It used to be the one shared SystemPrincipal, which merged
	// every connector's facts under one actor (and one supersession key).
	connector string
}

// defaultIngestSource is the id part of the source principal when the document
// names no source type.
const defaultIngestSource = "ingest"

// forConnector returns an ingestor that shares k's (late-bound) processor and
// stamps connector as the source. buildPlugins hands each plugin one named after
// its manifest id, so a plugin can only write as its own source — the same
// bounding scopedAgentRegistrar applies to agent registration.
func (k *kernelMemoryIngestor) forConnector(connector string) ReactiveMemoryIngestor {
	return &scopedMemoryIngestor{base: k, connector: connector}
}

// scopedMemoryIngestor is kernelMemoryIngestor bound to one connector. It holds
// the base by pointer because the processor is filled in after plugins Build.
type scopedMemoryIngestor struct {
	base      *kernelMemoryIngestor
	connector string
}

func (s *scopedMemoryIngestor) Ingest(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	return s.base.ingestAs(ctx, s.connector, doc)
}

// Ingest runs one document through the standard pipeline.
func (k *kernelMemoryIngestor) Ingest(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	if k == nil {
		return k.ingestAs(ctx, "", doc)
	}
	return k.ingestAs(ctx, k.connector, doc)
}

// ingestPrincipal is the source principal an ingest by connector runs as: the
// document's (plugin-set, kernel-owned) SourceType is the source id.
func ingestPrincipal(connector string, doc domain.ExternalDocument) domain.PrincipalRef {
	id := doc.SourceType
	if id == "" {
		id = defaultIngestSource
	}
	return domain.SourcePrincipal(connector, id)
}

func (k *kernelMemoryIngestor) ingestAs(ctx context.Context, connector string, doc domain.ExternalDocument) (string, error) {
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
	p := ingestPrincipal(connector, doc)
	if p.IsZero() {
		return "", fmt.Errorf("ingest: no connector names the source of this document")
	}
	// The writer of record is the source, kernel-stamped (domain.ExternalDocument.Author):
	// it becomes the chunks' source_agent_id and the facts' actor. What the plugin
	// put in Author is its descriptive label, kept as AuthorLabel.
	if doc.AuthorLabel == "" {
		doc.AuthorLabel = doc.Author
	}
	doc.Author = p.WriterID()
	return k.processor.ProcessSync(domain.WithPrincipal(ctx, p), doc)
}
