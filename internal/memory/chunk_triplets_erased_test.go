package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// erasingTripletStore refuses every save as ErrSourceErased (the chunk was erased while
// its batch was in flight) and records the ingest origin each save carried.
type erasingTripletStore struct {
	*lockedTripletStore
	mu      sync.Mutex
	origins []time.Time
}

func (s *erasingTripletStore) SaveChunkTriplets(ctx context.Context, id string, _ []domain.ChunkTriplet) error {
	o, _ := domain.IngestOriginFrom(ctx)
	s.mu.Lock()
	s.origins = append(s.origins, o)
	s.mu.Unlock()
	return fmt.Errorf("chunk %s: %w", id, domain.ErrSourceErased)
}

// ADR-0137 D5: a triplet save refused because the source was erased is final — dropped,
// counted, never requeued, and not reported as a KG failure; the save carries the
// enqueuing ingest's origin so the store can fence it.
func TestChunkTripletsBatcher_SourceErasedIsDroppedNotFailed(t *testing.T) {
	store := &erasingTripletStore{lockedTripletStore: newLockedTripletStore()}
	b := newFailureBatcher(&scriptedExtractor{}, store, 3)
	b.Start(context.Background())
	origin := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	w := b.EnqueueTracked(domain.WithIngestOrigin(context.Background(), origin), chunkDocs("z", 2))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	b.Stop()
	if w.Failed() != 0 {
		t.Fatalf("failed = %d: an erased source is not a KG failure", w.Failed())
	}
	if b.SourceErasedDrops() != 2 {
		t.Fatalf("SourceErasedDrops = %d, want 2", b.SourceErasedDrops())
	}
	if failed, requeued := b.FailureStats(); failed != 0 || requeued != 0 {
		t.Fatalf("failed=%d requeued=%d: never requeue an erased source", failed, requeued)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.origins) != 2 || !store.origins[0].Equal(origin) {
		t.Fatalf("origins = %v, want the ingest origin on every save", store.origins)
	}
}
