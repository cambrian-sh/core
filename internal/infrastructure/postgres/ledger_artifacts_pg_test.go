package postgres

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/vault"
)

// ADR-0157 S4 against a real, migrated Postgres (PG_TEST_DSN through 0053).

// Migration 0053: an artifact entry's provenance round-trips (session, instance,
// derived_from, source_refs), and Find addresses artifacts by session and step.
func TestPg_LedgerArtifactProvenanceRoundTrip(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	later := time.Now().Add(time.Hour)
	e := pgEntry("led_pgled_art_prov", domain.LedgerArtifact, "pgled-run-art", "pgled-turn-art", 3, "", "report,1", later)
	e.SessionID, e.InstanceID = "pgled-sess", "inst-42"
	e.DerivedFrom = []string{"led_pgled_in_1", "led_pgled_in_2"}
	e.SourceRefs = []string{"chunk-9", "doc-9"}
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, e.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.SessionID != "pgled-sess" || got.InstanceID != "inst-42" ||
		strings.Join(got.DerivedFrom, ",") != "led_pgled_in_1,led_pgled_in_2" || strings.Join(got.SourceRefs, ",") != "chunk-9,doc-9" {
		t.Fatalf("provenance did not round-trip: %+v", got)
	}
	rows, err := st.Find(ctx, domain.LedgerQuery{SessionID: "pgled-sess", StepIndex: 3, HasStep: true, Kind: domain.LedgerArtifact})
	if err != nil || len(rows) != 1 || rows[0].ID != e.ID {
		t.Fatalf("find by session+step = %+v %v", rows, err)
	}
}

// ADR-0157 Q6 (owner 2026-11-18, ENG-1990): the retired bbolt artifact records
// expire unimported and their vault bodies are released through the ledger's one
// body-release path: a body no ledger entry names is deleted, a body an entry
// still names (the same bytes uploaded since) is kept, a body already gone is
// not an error, and a re-run is a no-op.
func TestPg_LedgerReleaseBodies(t *testing.T) {
	st, v, ctx := newLedgerStore(t)
	orphan, err := v.Store([]byte("pgled legacy orphan bytes"))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := v.Store([]byte("pgled legacy shared bytes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE run_id = 'pgled-release-run'`)
	})
	e := pgEntry("led_pgled_release_shared", domain.LedgerArtifact, "pgled-release-run", "pgled-release-turn", 0, "", "", time.Now().Add(time.Hour))
	e.BodyHash, e.ContentHash = shared, shared
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	gone := strings.Repeat("ab", 32) // a hash whose bytes were never stored
	deleted, failed, err := st.ReleaseBodies(ctx, []string{orphan, shared, gone, orphan})
	if err != nil || failed != 0 || deleted != 2 { // orphan + the already-absent one
		t.Fatalf("release = deleted %d failed %d err %v, want 2/0/nil", deleted, failed, err)
	}
	if _, err := v.Load(orphan); err == nil {
		t.Fatal("an unreferenced legacy body survived the release")
	}
	if b, err := v.Load(shared); err != nil || string(b) != "pgled legacy shared bytes" {
		t.Fatalf("a body a ledger entry still names was deleted: %q %v", b, err)
	}
	if _, failed, err := st.ReleaseBodies(ctx, []string{orphan}); err != nil || failed != 0 {
		t.Fatalf("re-release: failed %d err %v (want an idempotent no-op)", failed, err)
	}
}

// ADR-0157 D5.6: the ADR-0137 erase reaches run-ledger entries that quote the
// erased document (a recall naming its chunk), everything derived from them
// (source_refs is transitive), and queues their vault bodies; an unrelated entry
// and a body another surviving entry shares are kept. The worker then deletes the
// unshared body.
func TestPg_Erase_ReachesLedgerEntries(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	led := NewPgLedgerStore(a.pool, vault.NewArtifactVault(t.TempDir()))
	if !led.Ready(ctx) {
		t.Skip("ledger_entries 0053 columns missing (run `orchestrator migrate up` first)")
	}
	v := led.bodies.(*vault.ArtifactVault)
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE run_id = 'lctest-ledger-run'`)
	})
	seedLifecycleDoc(t, a, ks, ctx, "lctest-doc", "agent:a", erasureSecret, "dg-1", 2)

	later := time.Now().Add(time.Hour)
	put := func(id string, kind domain.LedgerKind, body string, refs ...string) {
		t.Helper()
		e := pgEntry(id, kind, "lctest-ledger-run", "lctest-turn", 0, "", body, later)
		e.SourceRefs = refs
		if err := led.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	put("led_lctest_recall", domain.LedgerToolResult, "recalled "+erasureSecret, "lctest-doc-chunk-1")
	put("led_lctest_art", domain.LedgerArtifact, "chart built from the recall", "lctest-doc-chunk-1", "lctest-doc")
	put("led_lctest_shared", domain.LedgerArtifact, "shared bytes", "lctest-doc")
	put("led_lctest_keep_shared", domain.LedgerArtifact, "shared bytes") // same body, unrelated
	put("led_lctest_unrelated", domain.LedgerToolResult, "unrelated", "some-other-doc")

	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-doc"}}))
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if rec.Counts["ledger_entries"] != 3 {
		t.Fatalf("ledger_entries erased = %d, want 3 (counts %v)", rec.Counts["ledger_entries"], rec.Counts)
	}
	var left []string
	rows, err := a.pool.Query(ctx, `SELECT id FROM ledger_entries WHERE run_id = 'lctest-ledger-run'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	sort.Strings(left)
	if strings.Join(left, ",") != "led_lctest_keep_shared,led_lctest_unrelated" {
		t.Fatalf("ledger entries left after erase = %v", left)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM memory_erasure_outbox WHERE request_id = '`+rec.RequestID+`' AND step = 'vault_delete'`); n != 3 {
		t.Fatalf("vault_delete steps = %d, want 3", n)
	}

	erasureworker.New(er, nil, erasureworker.Config{Poll: time.Hour, Vault: v}, nil).RunOnce(ctx)
	for body, want := range map[string]bool{
		"recalled " + erasureSecret:   false,
		"chart built from the recall": false,
		"shared bytes":                true, // a surviving entry still names it
		"unrelated":                   true,
	} {
		_, err := os.Stat(filepath.Join(v.VaultPath, hashOf(t, body)))
		if exists := err == nil; exists != want {
			t.Errorf("vault body %q exists=%v, want %v", body, exists, want)
		}
	}
}
