package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// StartMemoryWorker begins background tasks for Memory Consolidation and Forgetfulness (Decay).
// It blocks until ctx is cancelled.
func (a *Agent) StartMemoryWorker(ctx context.Context, dryRun bool) error {
	slog.Info("MemoryWorker starting", "dry_run", dryRun)

	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
		if next.Before(now) {
			next = next.Add(24 * time.Hour)
		}
		delay := next.Sub(now)

		slog.Info("MemoryWorker scheduled next cleanup", "in", delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			slog.Info("MemoryWorker shutting down")
			timer.Stop()
			return nil
		case <-timer.C:
			slog.Info("MemoryWorker triggering nightly consolidation and decay")
			a.RunCleanupTask(ctx, dryRun)
		}
	}
}

// RunCleanupTask removes forgotten memories from vector store based on Decay logic.
func (a *Agent) RunCleanupTask(ctx context.Context, dryRun bool) {
	slog.Info("MemoryWorker running cleanup task")

	limit := 100
	staleDocs, err := a.Manager.Store.GetStaleMemories(ctx, limit)
	if err != nil {
		slog.Error("MemoryWorker failed fetching candidates", "err", err)
		return
	}

	if len(staleDocs) == 0 {
		slog.Info("MemoryWorker no stale memories to clean")
		return
	}

	processedIDs := make(map[string]bool)
	var deletedCount int

	for _, doc := range staleDocs {
		if processedIDs[doc.ID] {
			continue
		}

		clusterRes, err := a.Manager.Query(ctx, doc.Text, 15)
		if err != nil {
			slog.Warn("MemoryWorker cluster lookup failed", "doc_id", doc.ID, "err", err)
			continue
		}

		var cluster []domain.SearchResult
		for _, c := range clusterRes {
			if c.Score > 0.85 && !processedIDs[c.Document.ID] {
				cluster = append(cluster, c)
			}
		}

		if len(cluster) > 1 {
			a.consolidateCluster(ctx, cluster, processedIDs, dryRun, &deletedCount)
		} else {
			a.decayLoner(ctx, doc, processedIDs, dryRun, &deletedCount)
		}
	}

	slog.Info("MemoryWorker cleanup finished", "erased_vectors", deletedCount)
}

func (a *Agent) consolidateCluster(ctx context.Context, cluster []domain.SearchResult, processedIDs map[string]bool, dryRun bool, deletedCount *int) {
	var notesB strings.Builder
	var ids []string

	for i, c := range cluster {
		fmt.Fprintf(&notesB, "Note %d: %s\n", i+1, c.Document.Text)
		ids = append(ids, c.Document.ID)
		processedIDs[c.Document.ID] = true
	}

	prompt := domain.PromptBuild(
		domain.PromptSystem(
			"You are Cambrian's memory consolidation engine.",
			"Resolve any conflicts between the notes, base your answer on the most recent decision.",
		),
		domain.PromptContext(notesB.String()),
		domain.PromptTask("Produce a single concise technical summary of these notes."),
		domain.PromptOutputSchemaString(500, "A single concise technical summary. No bullet points. No preamble."),
	)

	if dryRun {
		slog.Info("MemoryWorker [dry-run] will merge vectors", "count", len(ids), "ids", ids)
		*deletedCount += len(ids)
		return
	}

	summary, err := a.LLMClient.Generate(ctx, prompt)
	if err != nil {
		slog.Error("MemoryWorker consolidation LLM failed", "err", err)
		return
	}

	docID := fmt.Sprintf("mem-super-%d", time.Now().UnixNano())
	newMem := &domain.Document{
		ID:   docID,
		Text: strings.TrimSpace(summary),
		Metadata: map[string]interface{}{
			"timestamp":    time.Now().Format(time.RFC3339),
			"source_agent": "MemoryWorker",
			"tags":         []string{"consolidated", "super-vector"},
			"parent_ids":   ids,
		},
		ActivationStrength: 0.5,
	}

	if err := a.Manager.Ingest(ctx, newMem); err == nil {
		if delErr := a.Manager.Store.DeleteBatch(ctx, ids); delErr != nil {
			slog.Warn("MemoryWorker failed deleting merged vectors", "ids", ids, "err", delErr)
		} else {
			*deletedCount += len(ids)
			slog.Info("MemoryWorker merged vectors", "count", len(ids), "super_id", docID)
		}
	}
}

func (a *Agent) decayLoner(ctx context.Context, doc domain.Document, processedIDs map[string]bool, dryRun bool, deletedCount *int) {
	processedIDs[doc.ID] = true
	if doc.ActivationStrength < 0.3 && doc.AccessCount < 2 {
		if dryRun {
			slog.Info("MemoryWorker [dry-run] will delete loner memory", "doc_id", doc.ID)
			*deletedCount++
			return
		}

		if err := a.Manager.Store.Delete(ctx, doc.ID); err == nil {
			*deletedCount++
		}
	}
}
