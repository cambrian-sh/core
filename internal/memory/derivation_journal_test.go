package memory

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-2122: the derivation journal keeps fact and KG triplet jobs until they finish.
// These tests drive the journaler against an in-memory journal that mirrors the
// derivation_jobs semantics; the Postgres store and the restart E2E are proven in
// internal/infrastructure/postgres and internal/kernel (TestPg_*).

type fakeJournalRow struct {
	job       domain.DerivationJob
	attempts  int
	lastError string
	claimedAt time.Time
	next      time.Time
	dead      bool
}

type fakeJournal struct {
	mu         sync.Mutex
	rows       map[[2]string]*fakeJournalRow
	reschedule []rescheduleCall
	failRecord error
}

type rescheduleCall struct {
	key          domain.DerivationKey
	retryAt      time.Time
	countAttempt bool
	dead         bool
}

func newFakeJournal() *fakeJournal { return &fakeJournal{rows: map[[2]string]*fakeJournalRow{}} }

func jkey(kind domain.DerivationKind, doc string) [2]string { return [2]string{string(kind), doc} }

func (f *fakeJournal) RecordDerivations(_ context.Context, jobs []domain.DerivationJob, lease time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecord != nil {
		return f.failRecord
	}
	now := time.Now()
	for _, j := range jobs {
		f.rows[jkey(j.Kind, j.DocumentID)] = &fakeJournalRow{job: j, claimedAt: now, next: now.Add(lease)}
	}
	return nil
}

func (f *fakeJournal) CompleteDerivation(_ context.Context, k domain.DerivationKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[jkey(k.Kind, k.DocumentID)]; ok && r.job.ContentDigest == k.ContentDigest {
		delete(f.rows, jkey(k.Kind, k.DocumentID))
	}
	return nil
}

func (f *fakeJournal) RescheduleDerivation(_ context.Context, k domain.DerivationKey, retryAt time.Time, lastError string, countAttempt bool, maxAttempts int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[jkey(k.Kind, k.DocumentID)]
	if !ok || r.job.ContentDigest != k.ContentDigest {
		return false, nil
	}
	if countAttempt {
		r.attempts++
	}
	r.lastError, r.next = lastError, retryAt
	r.dead = maxAttempts > 0 && r.attempts >= maxAttempts
	f.reschedule = append(f.reschedule, rescheduleCall{key: k, retryAt: retryAt, countAttempt: countAttempt, dead: r.dead})
	return r.dead, nil
}

func (f *fakeJournal) ClaimDerivations(_ context.Context, kind domain.DerivationKind, since time.Time, limit int, lease time.Duration) ([]domain.DerivationJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var due []*fakeJournalRow
	for _, r := range f.rows {
		if r.job.Kind == kind && !r.dead && (r.claimedAt.Before(since) || !r.next.After(now)) {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].job.DocumentID < due[j].job.DocumentID })
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]domain.DerivationJob, 0, len(due))
	for _, r := range due {
		r.claimedAt, r.next = now, now.Add(lease)
		j := r.job
		j.Attempts = r.attempts
		out = append(out, j)
	}
	return out, nil
}

func (f *fakeJournal) DerivationBacklog(context.Context) (map[domain.DerivationKind]domain.DerivationBacklog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.DerivationKind]domain.DerivationBacklog{}
	for _, r := range f.rows {
		b := out[r.job.Kind]
		if r.dead {
			b.Dead++
		} else {
			b.Pending++
			if r.attempts > 0 {
				b.Retrying++
			}
		}
		out[r.job.Kind] = b
	}
	return out, nil
}

func (f *fakeJournal) row(kind domain.DerivationKind, doc string) (fakeJournalRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[jkey(kind, doc)]
	if !ok {
		return fakeJournalRow{}, false
	}
	return *r, true
}

// makeDue moves every row's retry into the past (the backoff elapsed).
func (f *fakeJournal) makeDue() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		r.next = time.Now().Add(-time.Second)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// journalChunks is one document's stored chunks (doc-j, chunks j-0, j-1).
func journalChunks() []domain.Document {
	mk := func(id, text string) domain.Document {
		return domain.Document{ID: id, Text: text, DocumentType: domain.DocTypeMnemonicFact,
			Metadata: map[string]any{"tags": []string{"tenant_a"}, "document_id": "doc-j", "source_agent_id": "alice"}}
	}
	return []domain.Document{mk("j-0", "passage 0 says the sky is blue."), mk("j-1", "passage 1 says grass is green.")}
}

func ptrs(docs []domain.Document) []*domain.Document {
	out := make([]*domain.Document, len(docs))
	for i := range docs {
		out[i] = &docs[i]
	}
	return out
}

func journalReader(docs []domain.Document) domain.VectorReader {
	return authz.NewEnforcingVectorStore(&laneChunkStore{docs: docs}, quietLog())
}

func ingestCtx() context.Context {
	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice"))
	ctx = withIngestEvidence(ctx, "ev-1")
	ctx = domain.WithIngestOrigin(ctx, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	return domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: "doc-j", ContentDigest: currentDigest})
}

func fastBatcher(ext domain.TripletExtractor, store domain.ChunkTripletsStore, requeues int) *ChunkTripletsBatcher {
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: 10 * time.Millisecond, LLMTimeout: 5 * time.Second, MaxRequeues: requeues,
	})
	b.UseExtractor(ext)
	return b
}

// A fact job whose LLM call fails is rescheduled with backoff (an attempt counted), and
// the recovery pass re-feeds it once due; it then completes with the facts the live job
// would have written — the journaled evidence id makes the fact ids identical.
func TestDerivationJournal_FactFailureIsRetriedWithBackoffThenCompletes(t *testing.T) {
	sub := newMemSubstrate()
	inner := windowGen()
	failing := true
	var mu sync.Mutex
	gen := &scriptedGen{answer: func(p string) (string, error) {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return "", errors.New("provider 429: rate limited")
		}
		return inner.answer(p)
	}}
	e := newTestExtractor(gen, sub, FactExtractorConfig{MaxPromptChars: 1})
	e.Start(context.Background())
	t.Cleanup(e.Stop)
	docs := journalChunks()
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), e, nil, DerivationJournalConfig{RetryBase: time.Minute, RetryMax: time.Hour})
	d.since = time.Now()

	ctx := ingestCtx()
	sub2 := d.recordIngest(ctx, "doc-j", currentDigest, []string{"j-0", "j-1"}, evt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), true, true)
	if _, ok := fj.row(domain.DerivationFacts, "doc-j"); !ok {
		t.Fatal("the fact job must be journaled before the enqueue")
	}
	if _, ok := fj.row(domain.DerivationKGTriplets, "doc-j"); ok {
		t.Fatal("no KG batcher: no KG job is journaled")
	}
	before := time.Now()
	e.enqueue(ctx, "doc-j", evt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), ptrs(docs), false, sub2.hook(domain.DerivationFacts))
	eventually(t, "the failure is rescheduled", func() bool { r, _ := fj.row(domain.DerivationFacts, "doc-j"); return r.attempts == 1 })
	r, _ := fj.row(domain.DerivationFacts, "doc-j")
	if r.next.Before(before.Add(time.Minute)) || r.next.After(time.Now().Add(time.Minute+time.Second)) || r.lastError == "" || r.dead {
		t.Fatalf("row = %+v: want retry after retry_base with the cause", r)
	}

	// Not due yet: the pass re-feeds nothing.
	if n := d.RecoverOnce(context.Background()); n != 0 {
		t.Fatalf("re-fed %d before the backoff elapsed", n)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	fj.makeDue()
	if n := d.RecoverOnce(context.Background()); n != 1 {
		t.Fatalf("re-fed %d, want 1", n)
	}
	eventually(t, "the replayed job completes", func() bool { _, ok := fj.row(domain.DerivationFacts, "doc-j"); return !ok })
	f, ok := sub.byValue("passage 0")
	if !ok || f.EvidenceID != "ev-1" || f.Actor != "alice" || len(f.Tags) != 1 || f.Tags[0] != "tenant_a" {
		t.Fatalf("fact = %+v ok=%v: want the journaled evidence, the recorded writer and the chunk's tags", f, ok)
	}
	if want := domain.FactItemID("ev-1", "j-0", f.SubjectKey, f.AttributeKey, f.ValueNorm); f.ID != want {
		t.Fatalf("fact id %s, want the live job's deterministic id %s", f.ID, want)
	}
	if st := d.Stats(); st.Recorded != 1 || st.Failed != 1 || st.Recovered != 1 || st.Completed != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// After a restart the first pass takes the rows the previous process left (claimed
// before this one started), for both kinds; a document with no stored chunk left is
// dropped from the journal without a call.
func TestDerivationJournal_RestartRecoversOrphanedJobsOfBothKinds(t *testing.T) {
	sub := newMemSubstrate()
	gen := windowGen()
	e := newTestExtractor(gen, sub, FactExtractorConfig{MaxPromptChars: 1})
	e.Start(context.Background())
	t.Cleanup(e.Stop)
	store := newLockedTripletStore()
	b := fastBatcher(&scriptedExtractor{}, store, 0)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	docs := journalChunks()
	fj := newFakeJournal()

	// The previous process journaled these, then died.
	prev := ingestCtx()
	old := NewDerivationJournaler(fj, journalReader(docs), e, b, DerivationJournalConfig{})
	old.recordIngest(prev, "doc-j", currentDigest, []string{"j-0", "j-1"}, evt(time.Now()), true, true)
	old.recordIngest(domain.WithSourceVersion(prev, domain.SourceVersion{DocumentID: "doc-gone", ContentDigest: "x"}),
		"doc-gone", "x", []string{"gone-0"}, evt(time.Now()), true, true)
	time.Sleep(2 * time.Millisecond)

	d := NewDerivationJournaler(fj, journalReader(docs), e, b, DerivationJournalConfig{})
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	eventually(t, "every orphaned job completes", func() bool {
		fj.mu.Lock()
		defer fj.mu.Unlock()
		return len(fj.rows) == 0
	})
	if !store.has("j-0") || !store.has("j-1") {
		t.Fatal("the replayed KG job must persist both chunks' triplets")
	}
	if _, ok := sub.byValue("passage 1"); !ok {
		t.Fatal("the replayed fact job must store the document's facts")
	}
	if mk, ok := sub.marker("doc-j"); !ok || mk.PassagesCovered != 2 {
		t.Fatalf("marker = %+v ok=%v", mk, ok)
	}
	if got := gen.calls.Load(); got != 2 {
		t.Fatalf("LLM calls = %d, want one per window (no call for the document without chunks)", got)
	}
}

// Stop releases a queued job without touching its row: the next process recovers it.
// A full queue defers the job (due again after retry_base, no attempt counted).
func TestDerivationJournal_StopReleasesAndQueueFullDefers(t *testing.T) {
	sub := newMemSubstrate()
	e := newTestExtractor(windowGen(), sub, FactExtractorConfig{QueueSize: 1})
	docs := journalChunks()
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), e, nil, DerivationJournalConfig{RetryBase: time.Minute})
	ctx := ingestCtx()
	for _, doc := range []string{"doc-a", "doc-b"} {
		dctx := domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: doc, ContentDigest: "v"})
		s := d.recordIngest(dctx, doc, "v", []string{"j-0"}, evt(time.Now()), false, true)
		e.enqueue(dctx, doc, evt(time.Now()), ptrs(docs[:1]), false, s.hook(domain.DerivationFacts))
	}
	fj.mu.Lock()
	calls := append([]rescheduleCall(nil), fj.reschedule...)
	fj.mu.Unlock()
	if len(calls) != 1 || calls[0].key.DocumentID != "doc-b" || calls[0].countAttempt {
		t.Fatalf("reschedules = %+v: want doc-b deferred without an attempt", calls)
	}
	if r, _ := fj.row(domain.DerivationFacts, "doc-b"); r.attempts != 0 || r.next.Before(time.Now().Add(50*time.Second)) {
		t.Fatalf("doc-b = %+v", r)
	}
	e.Stop() // never started: doc-a is released
	r, ok := fj.row(domain.DerivationFacts, "doc-a")
	if !ok || r.attempts != 0 || r.lastError != "" {
		t.Fatalf("doc-a = %+v ok=%v: a released job stays journaled, untouched", r, ok)
	}
	if st := d.Stats(); st.Deferred != 1 || st.Completed != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A job that keeps failing dies after max_attempts: never re-fed again, counted dead.
func TestDerivationJournal_DiesAfterMaxAttempts(t *testing.T) {
	sub := newMemSubstrate()
	gen := &scriptedGen{answer: func(string) (string, error) { return "", errors.New("provider 500") }}
	e := newTestExtractor(gen, sub, FactExtractorConfig{})
	e.Start(context.Background())
	t.Cleanup(e.Stop)
	docs := journalChunks()
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), e, nil, DerivationJournalConfig{MaxAttempts: 2, RetryBase: time.Millisecond, RetryMax: time.Millisecond})
	d.since = time.Now()
	ctx := ingestCtx()
	s := d.recordIngest(ctx, "doc-j", currentDigest, []string{"j-0", "j-1"}, evt(time.Now()), false, true)
	e.enqueue(ctx, "doc-j", evt(time.Now()), ptrs(docs), false, s.hook(domain.DerivationFacts))
	eventually(t, "first failure", func() bool { r, _ := fj.row(domain.DerivationFacts, "doc-j"); return r.attempts == 1 })
	time.Sleep(5 * time.Millisecond)
	if n := d.RecoverOnce(context.Background()); n != 1 {
		t.Fatalf("re-fed %d, want 1", n)
	}
	eventually(t, "dead", func() bool { r, _ := fj.row(domain.DerivationFacts, "doc-j"); return r.dead })
	time.Sleep(5 * time.Millisecond)
	if n := d.RecoverOnce(context.Background()); n != 0 {
		t.Fatalf("a dead job was re-fed (%d)", n)
	}
	st := d.Stats()
	if st.Died != 1 || st.Backlog[domain.DerivationFacts].Dead != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// KG: a batch whose extractor is down (after the batcher's own requeues) fails the
// document's job (retry with backoff); a later pass re-feeds it and it completes.
func TestDerivationJournal_KGOutageIsRetried(t *testing.T) {
	store := newLockedTripletStore()
	ext := &scriptedExtractor{failFirst: 1, err: errUnavailable}
	b := fastBatcher(ext, store, 0)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	docs := journalChunks()
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), nil, b, DerivationJournalConfig{RetryBase: time.Minute})
	d.since = time.Now()
	ctx := ingestCtx()
	s := d.recordIngest(ctx, "doc-j", currentDigest, []string{"j-0", "j-1"}, evt(time.Now()), true, true)
	if _, ok := fj.row(domain.DerivationFacts, "doc-j"); ok {
		t.Fatal("no fact extractor: no fact job is journaled")
	}
	b.enqueueDocument(ctx, ptrs(docs), false, s.hook(domain.DerivationKGTriplets))
	eventually(t, "the KG failure is rescheduled", func() bool { r, _ := fj.row(domain.DerivationKGTriplets, "doc-j"); return r.attempts == 1 })
	fj.makeDue()
	if n := d.RecoverOnce(context.Background()); n != 1 {
		t.Fatalf("re-fed %d", n)
	}
	eventually(t, "the KG job completes", func() bool { _, ok := fj.row(domain.DerivationKGTriplets, "doc-j"); return !ok })
	if !store.has("j-0") || !store.has("j-1") {
		t.Fatal("triplets must be persisted after the retry")
	}
}

// ENG-501: a replayed job carries the version it was journaled for, so a document
// revised since refuses its writes; that is final — the job completes (row removed),
// writes nothing and leaves no marker.
func TestDerivationJournal_ReplayOfARevisedDocumentIsFenced(t *testing.T) {
	sub := newMemSubstrate()
	gen := windowGen()
	e := NewFactExtractor(gen, &fakeEmbedder{}, authz.NewEnforcingKnowledgeWriter(versionFencedSubstrate{sub}, nil, quietLog()),
		authz.NewEnforcingFactItemReader(sub, quietLog()), FactExtractorConfig{EmbedModelID: "test-embedder", MaxPromptChars: 1})
	e.Start(context.Background())
	t.Cleanup(e.Stop)
	store := &versionFencedTripletStore{lockedTripletStore: newLockedTripletStore()}
	b := fastBatcher(&scriptedExtractor{}, store, 0)
	b.Start(context.Background())
	t.Cleanup(b.Stop)
	docs := journalChunks()
	fj := newFakeJournal()
	// Journaled for digest-v1; the document is now at currentDigest.
	old := domain.WithSourceVersion(ingestCtx(), domain.SourceVersion{DocumentID: "doc-j", ContentDigest: "digest-v1"})
	NewDerivationJournaler(fj, journalReader(docs), e, b, DerivationJournalConfig{}).
		recordIngest(old, "doc-j", "digest-v1", []string{"j-0", "j-1"}, evt(time.Now()), true, true)
	time.Sleep(2 * time.Millisecond)
	d := NewDerivationJournaler(fj, journalReader(docs), e, b, DerivationJournalConfig{})
	d.since = time.Now()
	if n := d.RecoverOnce(context.Background()); n != 2 {
		t.Fatalf("re-fed %d, want both kinds", n)
	}
	eventually(t, "both fenced jobs complete", func() bool {
		fj.mu.Lock()
		defer fj.mu.Unlock()
		return len(fj.rows) == 0
	})
	if store.has("j-0") || store.has("j-1") {
		t.Fatal("triplets of a retired version must be refused")
	}
	if _, ok := sub.marker("doc-j"); ok {
		t.Fatal("a retired version must leave no marker")
	}
	if e.Stats().StaleVersion != 1 || b.StaleVersionDrops() != 2 {
		t.Fatalf("stale facts=%d triplets=%d", e.Stats().StaleVersion, b.StaleVersionDrops())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, v := range store.versions {
		if v.ContentDigest != "digest-v1" {
			t.Fatalf("a replayed save carried %+v, want the journaled version", v)
		}
	}
}

// A job still queued in this process is never fed twice, even when its lease ran out.
func TestDerivationJournal_InflightJobIsNotFedTwice(t *testing.T) {
	sub := newMemSubstrate()
	e := newTestExtractor(windowGen(), sub, FactExtractorConfig{}) // not started: jobs wait
	docs := journalChunks()
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(docs), e, nil, DerivationJournalConfig{})
	d.since = time.Now()
	ctx := ingestCtx()
	s := d.recordIngest(ctx, "doc-j", currentDigest, []string{"j-0", "j-1"}, evt(time.Now()), false, true)
	e.enqueue(ctx, "doc-j", evt(time.Now()), ptrs(docs), false, s.hook(domain.DerivationFacts))
	fj.makeDue() // the lease ran out while the job waits in the queue
	if n := d.RecoverOnce(context.Background()); n != 0 {
		t.Fatalf("an in-flight job was fed again (%d)", n)
	}
	if got := len(e.queue); got != 1 {
		t.Fatalf("queue = %d, want the one live job", got)
	}
	e.Stop()
}

// A failed journal write never fails the ingest: the job runs unjournaled.
func TestDerivationJournal_RecordFailureFallsBackToMemory(t *testing.T) {
	sub := newMemSubstrate()
	e := newTestExtractor(windowGen(), sub, FactExtractorConfig{})
	fj := newFakeJournal()
	fj.failRecord = errors.New("db down")
	d := NewDerivationJournaler(fj, journalReader(journalChunks()), e, nil, DerivationJournalConfig{})
	if s := d.recordIngest(ingestCtx(), "doc-j", currentDigest, []string{"j-0"}, evt(time.Now()), true, true); s != nil || s.hook(domain.DerivationFacts) != nil {
		t.Fatal("a failed journal write yields no submission and no hook")
	}
	if d.Stats().RecordFailed != 1 || len(d.inflight) != 0 {
		t.Fatalf("stats = %+v inflight = %d", d.Stats(), len(d.inflight))
	}
	e.Stop()
}

// A source outside execution.memory.fact_extraction_sources is never journaled.
func TestDerivationJournal_FilteredSourceIsNotJournaled(t *testing.T) {
	e := newTestExtractor(windowGen(), newMemSubstrate(), FactExtractorConfig{ExtractionSources: []string{"user"}})
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(nil), e, nil, DerivationJournalConfig{})
	if s := d.recordIngest(ingestCtx(), "doc-j", currentDigest, []string{"j-0"}, evt(time.Now()), true, true); s != nil {
		t.Fatal("an agent write with sources=[user] must not be journaled")
	}
	if _, ok := fj.row(domain.DerivationFacts, "doc-j"); ok {
		t.Fatal("journaled a filtered source")
	}
	e.Stop()
}

func TestDerivationTracker_FoldsChunkOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []derivationOutcome
		want derivationOutcome
	}{
		{"all done", []derivationOutcome{derivationDone, derivationDone}, derivationDone},
		{"one deferred", []derivationOutcome{derivationDone, derivationDeferred}, derivationDeferred},
		{"failed beats deferred", []derivationOutcome{derivationDeferred, derivationFailed, derivationDone}, derivationFailed},
		{"released beats all", []derivationOutcome{derivationFailed, derivationReleased}, derivationReleased},
	} {
		var got []derivationOutcome
		tr := newDerivationTracker(len(tc.in), func(o derivationOutcome, _ error) { got = append(got, o) })
		for i, o := range tc.in {
			if len(got) != 0 {
				t.Fatalf("%s: fired after %d of %d", tc.name, i, len(tc.in))
			}
			tr.settle(o, nil)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: fired %v, want exactly [%v]", tc.name, got, tc.want)
		}
	}
	var fired int
	newDerivationTracker(0, func(derivationOutcome, error) { fired++ }).settleAll(0, derivationDone, nil)
	if fired != 1 {
		t.Fatalf("a zero-chunk document fires once, got %d", fired)
	}
	if newDerivationTracker(3, nil) != nil {
		t.Fatal("no hook, no tracker")
	}
}

func TestDerivationJournal_BackoffDoublesToTheCap(t *testing.T) {
	d := NewDerivationJournaler(newFakeJournal(), journalReader(nil), newTestExtractor(windowGen(), newMemSubstrate(), FactExtractorConfig{}), nil,
		DerivationJournalConfig{RetryBase: time.Second, RetryMax: 5 * time.Second})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if got := d.backoff(i + 1); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if NewDerivationJournaler(nil, journalReader(nil), d.facts, nil, DerivationJournalConfig{}) != nil ||
		NewDerivationJournaler(newFakeJournal(), journalReader(nil), nil, nil, DerivationJournalConfig{}) != nil {
		t.Fatal("no journal or no queue: no journaler")
	}
	var nilJ *DerivationJournaler
	nilJ.Start(context.Background())
	nilJ.Stop()
	if nilJ.RecoverOnce(context.Background()) != 0 || nilJ.recordIngest(ingestCtx(), "d", "x", []string{"c"}, domain.SourceMeta{}, true, true) != nil {
		t.Fatal("a nil journaler is a no-op")
	}
}
