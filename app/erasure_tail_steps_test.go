package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/storage"
)

// ENG-1967: the composed async tail carries core's step_cache step (over the kernel's
// bbolt step cache) ahead of the plugins' steps, and only the plugins' steps when
// there is no step cache.
func TestCoreErasureTailSteps_StepCacheBeforePlugins(t *testing.T) {
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "agents.db"), 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sc, err := storage.NewBBoltStepCache(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Put(context.Background(), "k", &domain.Handoff{ID: "k"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	plugin := stubTailStep{name: "reactive_dead_letters"}
	steps := coreErasureTailSteps(&kernel.StorageHandle{StepCache: sc}, []domain.ErasureTailStep{plugin})
	if names := erasureTailStepNames(steps); len(names) != 2 || names[0] != storage.StepCacheErasureStepName || names[1] != "reactive_dead_letters" {
		t.Fatalf("tail steps = %v, want [step_cache reactive_dead_letters]", names)
	}
	if n, err := steps[0].RunErasureStep(context.Background(), domain.ErasedRecords{}); err != nil || n != 1 {
		t.Fatalf("composed step_cache step = (%d, %v), want the kernel cache's entry evicted", n, err)
	}
	if names := erasureTailStepNames(coreErasureTailSteps(&kernel.StorageHandle{}, []domain.ErasureTailStep{plugin})); len(names) != 1 {
		t.Fatalf("no step cache: tail steps = %v, want the plugin's only", names)
	}
}
