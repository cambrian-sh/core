package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2122 against a real, migrated Postgres: the derivation journal records a job per
// (kind, document), replaces it on a re-ingest, claims orphaned and due rows exactly
// once across concurrent claimers, completes and reschedules only the version it
// names, marks a job dead after its last attempt, counts the backlog, and loses a
// document's rows with the document's erasure.
func TestPg_DerivationJournal_Lifecycle(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	const doc, doc2 = "lctest-djdoc", "lctest-djdoc2"
	clean := func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM `+TableDerivationJobs+` WHERE document_id LIKE 'lctest-dj%'`)
	}
	clean()
	t.Cleanup(clean)

	origin := time.Date(2026, 11, 18, 1, 2, 3, 0, time.UTC)
	ref := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	job := domain.DerivationJob{
		Kind: domain.DerivationFacts, DocumentID: doc, ContentDigest: "dg-1", ChunkIDs: []string{doc + "-chunk-2", doc + "-chunk-1"},
		Writer: domain.AgentPrincipal("alice"), EvidenceID: "ev-1", IngestOrigin: origin,
		Source: domain.SourceMeta{SourceType: "file_drop", Title: "t", Author: "a", ReferenceTime: ref, ReferenceBasis: domain.ReferenceBasisEvent},
	}
	kg := job
	kg.Kind = domain.DerivationKGTriplets
	if err := a.RecordDerivations(ctx, []domain.DerivationJob{job, kg}, time.Hour); err != nil {
		t.Fatalf("RecordDerivations: %v", err)
	}

	// Fresh rows of the running process (claimed after since) are not taken.
	past := time.Now().Add(-time.Hour)
	if got, err := a.ClaimDerivations(ctx, domain.DerivationFacts, past, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("claimed a fresh, leased row: %v %v", got, err)
	}
	// dbNow is the database clock (claimed_at is written by it): a process that started
	// at dbNow() sees every row claimed before as orphaned.
	dbNow := func() time.Time {
		t.Helper()
		time.Sleep(20 * time.Millisecond)
		var n time.Time
		if err := a.pool.QueryRow(ctx, `SELECT now()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		return n
	}
	// A process started after the record sees them as orphans; the payload round-trips.
	since := dbNow()
	got, err := a.ClaimDerivations(ctx, domain.DerivationFacts, since, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("orphan claim: %+v %v", got, err)
	}
	g := got[0]
	if g.Kind != domain.DerivationFacts || g.DocumentID != doc || g.ContentDigest != "dg-1" || len(g.ChunkIDs) != 2 ||
		g.ChunkIDs[0] != doc+"-chunk-2" || g.Writer != job.Writer || g.EvidenceID != "ev-1" || !g.IngestOrigin.Equal(origin) ||
		g.Source.Title != "t" || !g.Source.ReferenceTime.Equal(ref) || g.Source.ReferenceBasis != domain.ReferenceBasisEvent || g.Attempts != 0 {
		t.Fatalf("claimed job = %+v, want %+v", g, job)
	}

	// Concurrent claimers never take the same row (FOR UPDATE SKIP LOCKED).
	if err := a.RecordDerivations(ctx, []domain.DerivationJob{{Kind: domain.DerivationFacts, DocumentID: doc2, ContentDigest: "x", ChunkIDs: []string{"c"}}}, 0); err != nil {
		t.Fatal(err)
	}
	since = dbNow()
	var mu sync.Mutex
	taken := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := a.ClaimDerivations(ctx, domain.DerivationFacts, since, 10, time.Hour)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			for _, j := range jobs {
				taken[j.DocumentID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if taken[doc] != 1 || taken[doc2] != 1 {
		t.Errorf("concurrent claimers took %v, want each orphan exactly once", taken)
	}
	for id, n := range taken {
		if n != 1 {
			t.Errorf("%s claimed %d times by concurrent claimers", id, n)
		}
	}

	// Reschedule: an older version is untouched; the current one backs off, then dies.
	if dead, err := a.RescheduleDerivation(ctx, domain.DerivationKey{Kind: domain.DerivationFacts, DocumentID: doc, ContentDigest: "dg-0"}, time.Now(), "x", true, 2); err != nil || dead {
		t.Fatalf("reschedule of another version: dead=%v err=%v", dead, err)
	}
	key := job.Key()
	if dead, err := a.RescheduleDerivation(ctx, key, time.Now().Add(-time.Second), "provider 429", true, 2); err != nil || dead {
		t.Fatalf("first failure: dead=%v err=%v", dead, err)
	}
	got, _ = a.ClaimDerivations(ctx, domain.DerivationFacts, past, 10, time.Minute)
	if len(got) != 1 || got[0].DocumentID != doc || got[0].Attempts != 1 {
		t.Fatalf("a due retry must be claimable with its attempts: %+v", got)
	}
	if b, err := a.DerivationBacklog(ctx); err != nil || b[domain.DerivationFacts].Retrying < 1 || b[domain.DerivationKGTriplets].Pending < 1 {
		t.Fatalf("backlog = %+v err %v", b, err)
	}
	if dead, err := a.RescheduleDerivation(ctx, key, time.Now().Add(-time.Second), "provider 429", true, 2); err != nil || !dead {
		t.Fatalf("second failure at max_attempts=2: dead=%v err=%v", dead, err)
	}
	got, _ = a.ClaimDerivations(ctx, domain.DerivationFacts, dbNow(), 10, time.Minute)
	for _, j := range got {
		if j.DocumentID == doc {
			t.Fatalf("a dead job was claimed: %+v", j)
		}
	}
	// ENG-2111: progress shrinks the KG row's chunk ids, for its own version only.
	if err := a.AdvanceDerivation(ctx, domain.DerivationKey{Kind: domain.DerivationKGTriplets, DocumentID: doc, ContentDigest: "dg-0"}, nil); err != nil {
		t.Fatalf("advance of another version: %v", err)
	}
	if err := a.AdvanceDerivation(ctx, kg.Key(), []string{doc + "-chunk-1"}); err != nil {
		t.Fatalf("AdvanceDerivation: %v", err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM `+TableDerivationJobs+` WHERE document_id = '`+doc+`' AND kind = 'kg_triplets' AND chunk_ids = ARRAY['`+doc+`-chunk-1']`); n != 1 {
		t.Fatal("the KG row's chunk ids are not the remaining chunk")
	}
	// A deferral counts no attempt (on the KG row).
	if dead, err := a.RescheduleDerivation(ctx, kg.Key(), time.Now().Add(time.Hour), "queue full", false, 1); err != nil || dead {
		t.Fatalf("deferral: dead=%v err=%v", dead, err)
	}

	// A re-ingest of the document replaces its row and revives it.
	job2 := job
	job2.ContentDigest = "dg-2"
	if err := a.RecordDerivations(ctx, []domain.DerivationJob{job2}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM `+TableDerivationJobs+` WHERE document_id = '`+doc+`' AND kind = 'facts' AND content_digest = 'dg-2' AND attempts = 0 AND dead_at IS NULL`); n != 1 {
		t.Fatalf("re-ingest row count = %d", n)
	}
	// Completion of the old version leaves the new one; the new one's completion removes it.
	if err := a.CompleteDerivation(ctx, key); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM `+TableDerivationJobs+` WHERE document_id = '`+doc+`' AND kind = 'facts'`); n != 1 {
		t.Fatal("completing a retired version removed the new version's job")
	}
	if err := a.CompleteDerivation(ctx, job2.Key()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM `+TableDerivationJobs+` WHERE document_id = '`+doc+`' AND kind = 'facts'`); n != 0 {
		t.Fatal("the completed job is still journaled")
	}

	// Erasure of the document takes its remaining rows (they carry its title/author).
	seedLifecycleDoc(t, a, ks, ctx, doc, "agent:a", "journal erasure text", "dg-2", 1)
	rec, err := er.Erase(ctx, erasureReq("lctest-djop", domain.ErasureSelector{DocumentIDs: []string{doc}}))
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if rec.Counts[TableDerivationJobs] != 1 {
		t.Errorf("erasure counts = %v, want derivation_jobs=1 (the KG row)", rec.Counts)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM `+TableDerivationJobs+` WHERE document_id = '`+doc+`'`); n != 0 {
		t.Fatalf("%d journal rows survived the erasure", n)
	}
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM memory_erasures WHERE principal = 'lctest-djop'`)
	})
}

// ENG-2143 against a real Postgres: an embed job stages its chunks and total, renews
// its progress and claim, reports a gone row, and IngestStatus reads accepted →
// embedding → failed / searchable for the document's current version only.
func TestPg_DerivationJournal_EmbedJobProgressAndStatus(t *testing.T) {
	a, _, _, ctx := newErasureStore(t)
	const doc = "lctest-djembed"
	clean := func() {
		c := context.Background()
		_, _ = a.pool.Exec(c, `DELETE FROM `+TableDerivationJobs+` WHERE document_id = $1`, doc)
		_, _ = a.pool.Exec(c, `DELETE FROM `+TableDocuments+` WHERE id = $1`, doc)
	}
	clean()
	t.Cleanup(clean)

	if _, found, err := a.IngestStatus(ctx, doc); err != nil || found {
		t.Fatalf("unknown document: found=%v err=%v", found, err)
	}
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: doc, Title: "t", SourceType: "file_drop", Text: "body",
		WrittenBy: "alice", ContentDigest: "dg-1"}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	if st, found, err := a.IngestStatus(ctx, doc); err != nil || !found || st.State != domain.IngestSearchable || st.Writer != "alice" {
		t.Fatalf("a document without an embed job = %+v found=%v err=%v; want searchable, writer alice", st, found, err)
	}
	ids := []string{doc + "-chunk-1", doc + "-chunk-2", doc + "-chunk-3"}
	job := domain.DerivationJob{Kind: domain.DerivationEmbed, DocumentID: doc, ContentDigest: "dg-1", ChunkIDs: ids,
		Writer: domain.AgentPrincipal("alice"), Staged: []byte(`{"chunks":[{"id":"x","text":"y"}]}`), ChunksTotal: 3}
	if err := a.RecordDerivations(ctx, []domain.DerivationJob{job}, 0); err != nil {
		t.Fatalf("RecordDerivations: %v", err)
	}
	if st, _, _ := a.IngestStatus(ctx, doc); st.State != domain.IngestAccepted || st.ChunksTotal != 3 || st.ChunksEmbedded != 0 {
		t.Fatalf("accepted = %+v", st)
	}
	got, err := a.ClaimDerivations(ctx, domain.DerivationEmbed, time.Now().Add(-time.Hour), 5, time.Minute)
	if err != nil || len(got) != 1 || got[0].ChunksTotal != 3 || string(got[0].Staged) == "" || len(got[0].ChunkIDs) != 3 {
		t.Fatalf("claim of a due embed job = %+v, %v", got, err)
	}
	var staged map[string]any
	if err := json.Unmarshal(got[0].Staged, &staged); err != nil || staged["chunks"] == nil {
		t.Fatalf("staged state did not round-trip: %s (%v)", got[0].Staged, err)
	}
	live, err := a.RenewDerivation(ctx, job.Key(), ids[1:], time.Hour)
	if err != nil || !live {
		t.Fatalf("RenewDerivation = %v, %v", live, err)
	}
	if st, _, _ := a.IngestStatus(ctx, doc); st.State != domain.IngestEmbedding || st.ChunksEmbedded != 1 {
		t.Fatalf("embedding = %+v", st)
	}
	var due bool
	if err := a.pool.QueryRow(ctx, `SELECT next_attempt_at > now() + interval '50 minutes' FROM `+TableDerivationJobs+` WHERE document_id = $1`, doc).Scan(&due); err != nil || !due {
		t.Fatalf("renewal must extend the claim: %v %v", due, err)
	}
	// Another version's key renews nothing and reports the row gone.
	if live, err := a.RenewDerivation(ctx, domain.DerivationKey{Kind: domain.DerivationEmbed, DocumentID: doc, ContentDigest: "dg-0"}, nil, time.Hour); err != nil || live {
		t.Fatalf("renewal of another version = %v, %v; want not live", live, err)
	}
	// A dead job reads failed with its reason.
	if dead, err := a.RescheduleDerivation(ctx, job.Key(), time.Now(), "embedding returned no vector", true, 1); err != nil || !dead {
		t.Fatalf("RescheduleDerivation = %v, %v", dead, err)
	}
	if st, _, _ := a.IngestStatus(ctx, doc); st.State != domain.IngestFailed || st.Error != "embedding returned no vector" {
		t.Fatalf("failed = %+v", st)
	}
	// A revise moved the document to another version: the old job is not its state.
	if _, err := a.pool.Exec(ctx, `UPDATE `+TableDocuments+` SET content_digest = 'dg-2' WHERE id = $1`, doc); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := a.IngestStatus(ctx, doc); st.State != domain.IngestSearchable {
		t.Fatalf("status with a superseded job = %+v, want searchable", st)
	}
	// Completion deletes the row; renewal then reports it gone.
	if err := a.CompleteDerivation(ctx, job.Key()); err != nil {
		t.Fatal(err)
	}
	if live, err := a.RenewDerivation(ctx, job.Key(), nil, time.Hour); err != nil || live {
		t.Fatalf("renewal after completion = %v, %v", live, err)
	}
}

// ENG-2139 against a real Postgres: a job past its last attempt is listed (key,
// attempts, last error, chunk count, never the payload), a requeue makes exactly
// that version live and due again, and a second requeue is a no-op.
func TestPg_DerivationJournal_DeadListAndRequeue(t *testing.T) {
	a, _, _, ctx := newErasureStore(t)
	const doc = "lctest-djdead"
	clean := func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM `+TableDerivationJobs+` WHERE document_id LIKE 'lctest-djdead%'`)
	}
	clean()
	t.Cleanup(clean)
	job := domain.DerivationJob{Kind: domain.DerivationKGTriplets, DocumentID: doc, ContentDigest: "dg-1",
		ChunkIDs: []string{doc + "-c1", doc + "-c2", doc + "-c3"}, Writer: domain.AgentPrincipal("alice")}
	if err := a.RecordDerivations(ctx, []domain.DerivationJob{job}, time.Hour); err != nil {
		t.Fatal(err)
	}
	dead, err := a.RescheduleDerivation(ctx, job.Key(), time.Now(), "provider 503", true, 1)
	if err != nil || !dead {
		t.Fatalf("job did not die: dead=%v err=%v", dead, err)
	}
	list, err := a.DeadDerivations(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var mine *domain.DeadDerivation
	for i := range list {
		if list[i].Key.DocumentID == doc {
			mine = &list[i]
		}
	}
	if mine == nil || mine.Key != job.Key() || mine.Attempts != 1 || mine.LastError != "provider 503" ||
		mine.ChunkCount != 3 || mine.DeadAt.IsZero() {
		t.Fatalf("dead job = %+v", mine)
	}
	if ok, err := a.RequeueDerivation(ctx, domain.DerivationKey{Kind: job.Kind, DocumentID: doc, ContentDigest: "other"}); err != nil || ok {
		t.Fatalf("another version requeued: ok=%v err=%v", ok, err)
	}
	if ok, err := a.RequeueDerivation(ctx, job.Key()); err != nil || !ok {
		t.Fatalf("requeue: ok=%v err=%v", ok, err)
	}
	if ok, _ := a.RequeueDerivation(ctx, job.Key()); ok {
		t.Fatal("a second requeue must be a no-op")
	}
	var attempts int
	var deadAt *time.Time
	var due bool
	if err := a.pool.QueryRow(ctx, `SELECT attempts, dead_at, next_attempt_at <= now() FROM `+TableDerivationJobs+`
		WHERE kind = $1 AND document_id = $2`, string(job.Kind), doc).Scan(&attempts, &deadAt, &due); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || deadAt != nil || !due {
		t.Fatalf("requeued row: attempts=%d dead_at=%v due=%v", attempts, deadAt, due)
	}
}
