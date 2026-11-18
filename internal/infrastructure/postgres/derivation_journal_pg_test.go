package postgres

import (
	"context"
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
