package memory

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// StartMemoryWorker begins the nightly background decay pass (Forgetfulness).
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
			slog.Info("MemoryWorker triggering nightly decay")
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

	// Every stale document goes through decay. The LLM CONSOLIDATION pass that used
	// to run here for near-duplicate clusters was removed: it merged a cluster into a
	// single unvalidated LLM summary and then DELETED the originals, so a bad summary
	// permanently lost the sources, and nothing checked it. Decay is the conservative
	// half — it deletes only what is both unreinforced and unread.
	for _, doc := range staleDocs {
		if processedIDs[doc.ID] {
			continue
		}
		a.decayLoner(ctx, doc, processedIDs, dryRun, &deletedCount)
	}

	slog.Info("MemoryWorker cleanup finished", "erased_vectors", deletedCount)
}

func (a *Agent) decayLoner(ctx context.Context, doc domain.Document, processedIDs map[string]bool, dryRun bool, deletedCount *int) {
	processedIDs[doc.ID] = true
	if doc.DocumentType == domain.DocTypeMnemonicEntity {
		if kind, _ := doc.Metadata["kind"].(string); kind == "source_document" {
			if cid, _ := doc.Metadata["content_cid"].(string); cid != "" {
				return // source-doc entities are GC-exempt (ADR-0060 D8)
			}
		}
	}
	// A row that hangs from an episode is NOT a loner — its retention is the
	// EPISODE's operation (ADR-0095 D6: forget an episode = delete the `experiences`
	// parent, the FK cascade takes its chunks). The reverse is the invariant this
	// guard defends: while the episode lives, its records live. Experiential rows
	// are born at activation 0.1 (scenes up to 0.5 with surprise), so without this
	// the nightly pass would silently shred the rarely-recalled half of every
	// episode and contradict ADR-0049 D2 ("actions are durable, the transition log")
	// — leaving an `experiences` parent whose transition log has holes in it.
	//
	// Unparented experiential rows (empty ExperienceID) stay GC-able exactly as
	// before: nothing owns their lifetime, so the loner heuristic is still the only
	// thing that can reclaim them.
	if doc.ExperienceID != "" {
		return
	}
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
