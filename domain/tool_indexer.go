package domain

import "context"

// ToolIndexer writes a tool's embedding document into the vector store so the
// VectorToolRetriever can rank it (ADR-0044 D8). It builds the deterministic
// document (ToolDocBuilder), embeds it with the kernel embedder, and upserts it
// as a DocTypeTool keyed by the tool's identity. Called at discovery — native
// tools at LoadRegistry, MCP tools at tools/list — and on MCP re-sync.
type ToolIndexer struct {
	Store    VectorStore
	Embedder Embedder
}

// Index embeds and upserts one tool's document (keyed by tool name / identity).
func (ix *ToolIndexer) Index(ctx context.Context, tool SystemTool) error {
	doc := BuildToolDoc(tool)
	vec, err := ix.Embedder.Embed(ctx, doc)
	if err != nil {
		return err
	}
	return ix.Store.Save(ctx, &Document{
		ID:           tool.Name,
		DocumentType: DocTypeTool,
		Text:         doc,
		Embedding:    Embedding{Vector: vec},
	})
}

// IndexAll indexes a batch of tools, returning the first error. A failure on one
// tool does not abandon the rest — they are all attempted (best-effort indexing
// at discovery), and the first error is returned for visibility.
func (ix *ToolIndexer) IndexAll(ctx context.Context, tools []SystemTool) error {
	var firstErr error
	for _, t := range tools {
		if err := ix.Index(ctx, t); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove drops a tool's document from the index (used on MCP re-sync when a
// server stops advertising a tool).
func (ix *ToolIndexer) Remove(ctx context.Context, toolName string) error {
	return ix.Store.Delete(ctx, toolName)
}
