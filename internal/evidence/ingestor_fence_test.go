package evidence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// fencedStore is a fakeStore that also offers the ADR-0137 D5 early erasure answer:
// a source key erased at erasedAt is refused for any origin before it.
type fencedStore struct {
	*fakeStore
	erased map[string]time.Time
}

func (f *fencedStore) SourceErased(ctx context.Context, sourceKey string) (bool, error) {
	origin, ok := domain.IngestOriginFrom(ctx)
	at, erased := f.erased[sourceKey]
	return ok && erased && at.After(origin), nil
}

// ENG-1866: a replayed delivery whose source was erased after its origin is refused
// with ErrSourceErased BEFORE its bytes reach the content store (the erase already
// deleted them there), and no evidence row or outbox item is written.
func TestIngest_ErasedSourceRefusedBeforeBytes(t *testing.T) {
	blobs := newFakeBlobs()
	erasedAt := time.Now().UTC()
	store := &fencedStore{fakeStore: newFakeStore(), erased: map[string]time.Time{"doc-1": erasedAt}}
	ing, err := NewIngestor(blobs, store)
	if err != nil {
		t.Fatal(err)
	}
	raw := Raw{SourceID: "watch:s", SourceKey: "doc-1", SourceRevision: "r1", Bytes: []byte("erased words")}

	replay := domain.WithIngestOrigin(context.Background(), erasedAt.Add(-time.Minute))
	if _, _, err := ing.Ingest(replay, raw); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replay before the erase: want ErrSourceErased, got %v", err)
	}
	if blobs.putCalls != 0 || len(store.rows) != 0 || len(store.outbox) != 0 {
		t.Fatalf("a refused replay wrote: puts=%d rows=%d outbox=%d", blobs.putCalls, len(store.rows), len(store.outbox))
	}

	// A delivery accepted after the erase is a new act (ADR-0137 Q4).
	fresh := domain.WithIngestOrigin(context.Background(), erasedAt.Add(time.Minute))
	if _, inserted, err := ing.Ingest(fresh, raw); err != nil || !inserted {
		t.Fatalf("new act after the erase: inserted=%v err=%v", inserted, err)
	}
	// No origin on ctx (a kernel-authored write) is never refused by the early check.
	if _, _, err := ing.Ingest(context.Background(), Raw{SourceID: "watch:s", SourceKey: "doc-1", SourceRevision: "r2", Bytes: []byte("x")}); err != nil {
		t.Fatalf("no origin: %v", err)
	}
}
