package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2111: journaled KG jobs are fed to the batcher in slices, round-robin across
// documents, with the row's progress written after each slice. These tests drive the
// journaler against the in-memory journal of derivation_journal_test.go; the Postgres
// E2E is internal/kernel TestPg_KGFeed_*.

// orderExtractor records the chunk ids it extracted, in order. The first call blocks
// until gate is closed (nil = no gate); while failOn is set, a batch holding that chunk
// fails (not as unavailable, so the batcher does not re-queue it).
type orderExtractor struct {
	mu     sync.Mutex
	order  []string
	gate   chan struct{}
	once   sync.Once
	failOn string
}

func (e *orderExtractor) ExtractBatch(_ context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	if e.gate != nil {
		e.once.Do(func() { <-e.gate })
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		if e.failOn != "" && id == e.failOn {
			return nil, errors.New("extractor: malformed reply")
		}
	}
	e.order = append(e.order, ids...)
	out := make([][]domain.ChunkTriplet, len(texts))
	for i := range texts {
		out[i] = []domain.ChunkTriplet{{H: ids[i], R: "mentions", T: "x"}}
	}
	return out, nil
}

func (e *orderExtractor) setFailOn(id string) {
	e.mu.Lock()
	e.failOn = id
	e.mu.Unlock()
}

// positions returns each id's first extraction index and how often it was extracted.
func (e *orderExtractor) positions() (map[string]int, map[string]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	first, count := map[string]int{}, map[string]int{}
	for i, id := range e.order {
		if _, ok := first[id]; !ok {
			first[id] = i
		}
		count[id]++
	}
	return first, count
}

func docChunks(doc string, n int) []domain.Document {
	out := make([]domain.Document, n)
	for i := range out {
		out[i] = domain.Document{ID: fmt.Sprintf("%s-%d", doc, i), Text: fmt.Sprintf("%s passage %d.", doc, i),
			DocumentType: domain.DocTypeMnemonicFact,
			Metadata:     map[string]any{"tags": []string{"tenant_a"}, "document_id": doc, "source_agent_id": "alice"}}
	}
	return out
}

func idsOf(docs []domain.Document) []string {
	out := make([]string, len(docs))
	for i := range docs {
		out[i] = docs[i].ID
	}
	return out
}

func feedBatcher(ext domain.TripletExtractor, store domain.ChunkTripletsStore, queue, batch int) *ChunkTripletsBatcher {
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: queue, BatchSize: batch, MaxIdle: 5 * time.Millisecond, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)
	return b
}

// ingestKG journals and feeds one document's KG job the way IngestionManager does.
func ingestKG(t *testing.T, d *DerivationJournaler, doc string, docs []domain.Document, track bool) *TripletsWait {
	t.Helper()
	ctx := domain.WithSourceVersion(ingestCtx(), domain.SourceVersion{DocumentID: doc, ContentDigest: "v1"})
	s := d.recordIngest(ctx, doc, "v1", idsOf(docs), evt(time.Now()), true, false)
	w, ok := s.enqueueKG(ptrs(docs), track)
	if !ok {
		t.Fatalf("%s: KG job not journaled", doc)
	}
	return w
}

func journalEmpty(fj *fakeJournal) bool {
	fj.mu.Lock()
	defer fj.mu.Unlock()
	return len(fj.rows) == 0
}

// A small document ingested while a large one is being extracted gets its triplets
// after at most the large document's in-flight slices, not after the whole document;
// the large one still finishes, and nothing is dropped or deferred.
func TestKGFeed_SmallDocumentIsNotStarvedByALargeOne(t *testing.T) {
	ext := &orderExtractor{gate: make(chan struct{})}
	store := newLockedTripletStore()
	b := feedBatcher(ext, store, 64, 8)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	big, small := docChunks("big", 120), docChunks("small", 3)
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(append(append([]domain.Document{}, big...), small...)), nil, b,
		DerivationJournalConfig{KGSliceChunks: 8, KGSlicesInFlight: 1})
	d.since = time.Now()
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	ingestKG(t, d, "big", big, false)
	ingestKG(t, d, "small", small, false)
	b.mu.Lock()
	q := len(b.queue) + len(b.pending)
	b.mu.Unlock()
	if q > 8+len(small) {
		t.Fatalf("batcher holds %d chunks: the large document must not have more than one slice queued", q)
	}
	close(ext.gate)
	eventually(t, "both documents complete", func() bool { return journalEmpty(fj) })

	first, count := ext.positions()
	lastSmall := 0
	for _, c := range small {
		lastSmall = max(lastSmall, first[c.ID])
	}
	// The big document's first slice (8) was in flight; the small one goes next.
	if lastSmall >= 16 {
		t.Fatalf("small document's last chunk extracted at position %d of %d: it waited behind the large document", lastSmall, len(ext.order))
	}
	for _, c := range append(append([]domain.Document{}, big...), small...) {
		if !store.has(c.ID) || count[c.ID] != 1 {
			t.Fatalf("chunk %s: stored=%v extracted %d times, want once", c.ID, store.has(c.ID), count[c.ID])
		}
	}
	if st := d.Stats(); st.Deferred != 0 || st.Completed != 2 || st.KGSlices != 16 {
		t.Fatalf("stats = %+v: want no deferral, 2 completions, 15+1 slices", st)
	}
	if _, dropped, _, _, _ := b.Stats(); dropped != 0 {
		t.Fatalf("batcher dropped %d chunks", dropped)
	}
}

// A document with more chunks than the batcher queue holds finishes (it used to be
// deferred again and again): slices are capped at the queue size, progress is written,
// and a sync ingest's wait spans every slice.
func TestKGFeed_DocumentLargerThanTheQueueCompletes(t *testing.T) {
	ext := &orderExtractor{}
	store := newLockedTripletStore()
	b := feedBatcher(ext, store, 16, 8)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	huge := docChunks("huge", 100)
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(huge), nil, b, DerivationJournalConfig{}) // slice 256 → capped at 16
	d.since = time.Now()
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	w := ingestKG(t, d, "huge", huge, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil || w.Failed() != 0 {
		t.Fatalf("sync wait = %v failed=%d", err, w.Failed())
	}
	eventually(t, "the row is completed", func() bool { return journalEmpty(fj) })
	for _, c := range huge {
		if !store.has(c.ID) {
			t.Fatalf("chunk %s has no triplets", c.ID)
		}
	}
	st := d.Stats()
	if st.Deferred != 0 || st.KGSlices != 7 || st.KGAdvanced == 0 {
		t.Fatalf("stats = %+v: want 7 slices of <= 16, progress written, no deferral", st)
	}
	if _, dropped, _, _, _ := b.Stats(); dropped != 0 {
		t.Fatalf("batcher dropped %d chunks", dropped)
	}
}

// A failed slice stops the document; the row keeps exactly the chunks not yet derived
// (the failed slice and the rest), so the retry resumes there and never re-extracts the
// slices that finished.
func TestKGFeed_FailureKeepsProgressAndTheRetryResumes(t *testing.T) {
	ext := &orderExtractor{failOn: "res-25"}
	store := newLockedTripletStore()
	b := feedBatcher(ext, store, 64, 10)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	docs := docChunks("res", 50)
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), nil, b,
		DerivationJournalConfig{KGSliceChunks: 10, KGSlicesInFlight: 1, RetryBase: time.Hour, RecoveryInterval: 10 * time.Millisecond})
	d.since = time.Now()
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	ingestKG(t, d, "res", docs, false)
	eventually(t, "the failure is rescheduled", func() bool { r, _ := fj.row(domain.DerivationKGTriplets, "res"); return r.attempts == 1 })
	r, _ := fj.row(domain.DerivationKGTriplets, "res")
	if got := r.job.ChunkIDs; len(got) != 30 || got[0] != "res-20" || got[29] != "res-49" {
		t.Fatalf("remaining = %v: want res-20..res-49", got)
	}
	ext.setFailOn("")
	fj.makeDue()
	eventually(t, "the retry completes", func() bool { return journalEmpty(fj) })
	_, count := ext.positions()
	for _, c := range docs {
		if !store.has(c.ID) || count[c.ID] != 1 {
			t.Fatalf("chunk %s: stored=%v extracted %d times, want once", c.ID, store.has(c.ID), count[c.ID])
		}
	}
}

// Stop leaves a document still waiting for its next slice in the journal with its
// progress, releases its sync waiter, and a later process finishes it from there.
func TestKGFeed_StopLeavesTheRestForTheNextProcess(t *testing.T) {
	ext := &orderExtractor{gate: make(chan struct{})}
	store := newLockedTripletStore()
	b := feedBatcher(ext, store, 64, 4)
	b.Start(context.Background())
	docs := docChunks("stop", 20)
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), nil, b, DerivationJournalConfig{KGSliceChunks: 4, KGSlicesInFlight: 1})
	d.since = time.Now()
	d.Start(context.Background())
	w := ingestKG(t, d, "stop", docs, true)
	d.Stop()
	close(ext.gate)
	b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Wait(ctx); !errors.Is(err, ErrTripletsBatcherStopped) {
		t.Fatalf("sync wait = %v, want released as stopped", err)
	}
	r, ok := fj.row(domain.DerivationKGTriplets, "stop")
	if !ok || r.attempts != 0 || len(r.job.ChunkIDs) != 16 || !strings.HasSuffix(r.job.ChunkIDs[0], "-4") {
		t.Fatalf("row = %+v ok=%v: want the 16 chunks after the finished slice, untouched otherwise", r, ok)
	}

	// The next process.
	b2 := feedBatcher(ext, store, 64, 4)
	b2.Start(context.Background())
	t.Cleanup(b2.Stop)
	time.Sleep(2 * time.Millisecond)
	d2 := NewDerivationJournaler(fj, journalReader(docs), nil, b2, DerivationJournalConfig{KGSliceChunks: 4, KGSlicesInFlight: 1})
	d2.Start(context.Background())
	t.Cleanup(d2.Stop)
	eventually(t, "the next process finishes the document", func() bool { return journalEmpty(fj) })
	_, count := ext.positions()
	for _, c := range docs {
		if !store.has(c.ID) || count[c.ID] != 1 {
			t.Fatalf("chunk %s: stored=%v extracted %d times, want once", c.ID, store.has(c.ID), count[c.ID])
		}
	}
}
