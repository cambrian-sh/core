package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-501 (ADR-0137 D5 / ADR-0138 D3): the async KG triplet and fact jobs carry the
// document version they were extracted from, and a write the store refuses because the
// document was revised since (domain.ErrSourceVersionRetired) is final — dropped,
// counted, never requeued, never a KG/fact failure, and no extraction marker is left for
// the new version's job to misread. The Postgres refusal itself and the full race are
// proven in internal/infrastructure/postgres and internal/kernel (TestPg_*).

const currentDigest = "digest-v2"

// versionFencedTripletStore refuses a save whose ctx names a version other than the
// current one, like the Postgres fence, and records the version each save carried.
type versionFencedTripletStore struct {
	*lockedTripletStore
	mu       sync.Mutex
	versions []domain.SourceVersion
}

func (s *versionFencedTripletStore) SaveChunkTriplets(ctx context.Context, id string, ts []domain.ChunkTriplet) error {
	v, _ := domain.SourceVersionFrom(ctx)
	s.mu.Lock()
	s.versions = append(s.versions, v)
	s.mu.Unlock()
	if v.ContentDigest != "" && v.ContentDigest != currentDigest {
		return fmt.Errorf("document %s: %w", v.DocumentID, domain.ErrSourceVersionRetired)
	}
	return s.lockedTripletStore.SaveChunkTriplets(ctx, id, ts)
}

func TestChunkTripletsBatcher_RetiredVersionIsDroppedNotFailed(t *testing.T) {
	store := &versionFencedTripletStore{lockedTripletStore: newLockedTripletStore()}
	b := newFailureBatcher(&scriptedExtractor{}, store, 3)
	b.Start(context.Background())
	t.Cleanup(b.Stop)

	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	old := domain.WithSourceVersion(domain.WithIngestOrigin(context.Background(), origin),
		domain.SourceVersion{DocumentID: "doc-r", ContentDigest: "digest-v1"})
	w := b.EnqueueTracked(old, chunkDocs("r", 2))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w.Failed() != 0 {
		t.Fatalf("failed = %d: a retired version is not a KG failure", w.Failed())
	}
	if got := b.StaleVersionDrops(); got != 2 {
		t.Fatalf("StaleVersionDrops = %d, want 2", got)
	}
	if failed, requeued := b.FailureStats(); failed != 0 || requeued != 0 {
		t.Fatalf("failed=%d requeued=%d: never requeue a retired version", failed, requeued)
	}
	if b.SourceErasedDrops() != 0 {
		t.Fatal("a retired version is not an erasure")
	}

	// The current version's chunks land.
	cur := domain.WithSourceVersion(context.Background(), domain.SourceVersion{DocumentID: "doc-r", ContentDigest: currentDigest})
	w = b.EnqueueTracked(cur, chunkDocs("r", 2))
	if err := w.Wait(waitCtx(t)); err != nil || w.Failed() != 0 {
		t.Fatalf("current version: err=%v failed=%d", err, w.Failed())
	}
	if _, _, _, _, persisted := b.Stats(); persisted == 0 {
		t.Fatal("the current version's triplets must be persisted")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.versions) != 4 || store.versions[0].DocumentID != "doc-r" || store.versions[0].ContentDigest != "digest-v1" ||
		store.versions[3].ContentDigest != currentDigest {
		t.Fatalf("versions = %+v, want the enqueuing ingest's version on every save", store.versions)
	}
}

// versionFencedSubstrate refuses item and marker writes of a retired version.
type versionFencedSubstrate struct {
	*memSubstrate
}

func (s versionFencedSubstrate) retired(ctx context.Context) error {
	if v, ok := domain.SourceVersionFrom(ctx); ok && v.ContentDigest != currentDigest {
		return fmt.Errorf("document %s: %w", v.DocumentID, domain.ErrSourceVersionRetired)
	}
	return nil
}

func (s versionFencedSubstrate) PutItem(ctx context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	if err := s.retired(ctx); err != nil {
		return "", false, err
	}
	return s.memSubstrate.PutItem(ctx, it)
}

func (s versionFencedSubstrate) MarkExtracted(ctx context.Context, mk domain.ExtractionMarker) error {
	if err := s.retired(ctx); err != nil {
		return err
	}
	return s.memSubstrate.MarkExtracted(ctx, mk)
}

func TestFactExtractor_RetiredVersionStopsWithoutMarker(t *testing.T) {
	sub := newMemSubstrate()
	fenced := versionFencedSubstrate{sub}
	gen := windowGen()
	e := NewFactExtractor(gen, &fakeEmbedder{}, authz.NewEnforcingKnowledgeWriter(fenced, nil, quietLog()),
		authz.NewEnforcingFactItemReader(sub, quietLog()), FactExtractorConfig{EmbedModelID: "test-embedder", MaxPromptChars: 1})
	src := evt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	docs := factChunkDocs("passage 0 says a thing.", "passage 1 says another.")

	old := domain.WithSourceVersion(context.Background(), domain.SourceVersion{DocumentID: "doc-1", ContentDigest: "digest-v1"})
	res, err := e.ExtractDocument(old, "doc-1", "", src, docs)
	if err != nil {
		t.Fatalf("a retired version is not an extraction failure: %v", err)
	}
	if res.Stats.StaleVersion != 1 || res.Stats.Inserted != 0 || res.Windows != 1 {
		t.Fatalf("result = %+v: the run must stop at the first refused write", res)
	}
	if gen.calls.Load() != 1 {
		t.Fatalf("calls = %d: no further window is extracted once the version is retired", gen.calls.Load())
	}
	if _, ok := sub.marker("doc-1"); ok {
		t.Fatal("a retired version must leave no extraction marker (the new version reuses the chunk ids)")
	}
	if st := e.Stats(); st.StaleVersion != 1 || st.FailedDocs != 0 {
		t.Fatalf("stats = %+v", st)
	}

	// The current version extracts both windows and marks complete.
	cur := domain.WithSourceVersion(context.Background(), domain.SourceVersion{DocumentID: "doc-1", ContentDigest: currentDigest})
	res, err = e.ExtractDocument(cur, "doc-1", "", src, docs)
	if err != nil || res.Skipped || res.Stats.Inserted != 2 || res.Stats.StaleVersion != 0 {
		t.Fatalf("current version: %+v err %v", res, err)
	}
	if mk, ok := sub.marker("doc-1"); !ok || mk.PassagesCovered != 2 {
		t.Fatalf("marker = %+v ok=%v", mk, ok)
	}
}

// A refused marker alone (a window with no facts) also ends the run.
func TestFactExtractor_RetiredVersionRefusedMarkerStops(t *testing.T) {
	sub := newMemSubstrate()
	gen := &scriptedGen{answer: func(string) (string, error) { return `{"facts":[]}`, nil }}
	e := NewFactExtractor(gen, &fakeEmbedder{}, authz.NewEnforcingKnowledgeWriter(versionFencedSubstrate{sub}, nil, quietLog()),
		authz.NewEnforcingFactItemReader(sub, quietLog()), FactExtractorConfig{EmbedModelID: "test-embedder", MaxPromptChars: 1})
	old := domain.WithSourceVersion(context.Background(), domain.SourceVersion{DocumentID: "doc-1", ContentDigest: "digest-v1"})
	res, err := e.ExtractDocument(old, "doc-1", "", evt(time.Now()), factChunkDocs("first.", "second."))
	if err != nil || res.Windows != 1 || gen.calls.Load() != 1 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, gen.calls.Load())
	}
	if e.Stats().StaleVersion != 1 {
		t.Fatalf("stats = %+v", e.Stats())
	}
}

func TestSourceVersionContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := domain.SourceVersionFrom(ctx); ok {
		t.Fatal("no version on a bare ctx")
	}
	if _, ok := domain.SourceVersionFrom(domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: "d"})); ok {
		t.Fatal("a version without a digest is ignored")
	}
	v := domain.SourceVersion{DocumentID: "d", ContentDigest: "x"}
	got, ok := domain.SourceVersionFrom(context.WithoutCancel(domain.WithSourceVersion(ctx, v)))
	if !ok || got != v {
		t.Fatalf("version must survive WithoutCancel: %+v %v", got, ok)
	}
}
