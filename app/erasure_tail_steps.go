package app

import (
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/storage"
)

// coreErasureTailSteps is the ADR-0137 D7 async tail as the erase transaction queues
// and the erasure worker runs it: core's own steps over kernel-held stores outside
// Postgres, then the plugins' (ENG-1966), all through domain.ErasureTailStep.
//
//   - step_cache (ENG-1967, R4 row 28): evicts the whole step cache. An entry
//     records neither its session nor the records it read (the key is a salted
//     hash, ADR-0145 D9), so a hand-off that quoted erased content cannot be singled
//     out; the cache is a TTL memo, so a flush costs only recomputation.
func coreErasureTailSteps(store *kernel.StorageHandle, plugins []domain.ErasureTailStep) []domain.ErasureTailStep {
	var out []domain.ErasureTailStep
	if store != nil && store.StepCache != nil {
		if p, ok := store.StepCache.(storage.StepCachePurger); ok {
			out = append(out, storage.StepCacheErasureStep{Cache: p})
		} else {
			// A step cache that cannot be purged would keep erased hand-offs; say so
			// at boot rather than drop the step silently (AGENTS.md §3.2).
			slog.Warn("ADR-0137: the step cache cannot be purged; erasures leave its entries until their TTL",
				"type", fmt.Sprintf("%T", store.StepCache))
		}
	}
	return append(out, plugins...)
}
