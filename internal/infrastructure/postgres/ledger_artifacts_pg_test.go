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

// ADR-0157 Q6: retired bbolt artifact records are imported as artifact entries
// with their own tags, session and step, naming the vault bytes they already
// had; the import is idempotent; the entries follow ledger retention.
func TestPg_LedgerImportLegacyArtifacts(t *testing.T) {
	st, v, ctx := newLedgerStore(t)
	hash, err := v.Store([]byte("legacy upload"))
	if err != nil {
		t.Fatal(err)
	}
	recs := []LegacyArtifact{{Hash: hash, ContentType: "text/plain", SizeBytes: 13, SessionID: "pgled-legacy-sess",
		StepIndex: 1, SemanticSummary: "old report", Tags: []string{"team_a"}}}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE session_id = 'pgled-legacy-sess'`)
	})
	now := time.Now()
	n, err := st.ImportLegacyArtifacts(ctx, recs, now, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	if n, err = st.ImportLegacyArtifacts(ctx, recs, now, time.Hour); err != nil || n != 1 {
		t.Fatalf("re-import: %d %v", n, err)
	}
	rows, err := st.Find(ctx, domain.LedgerQuery{SessionID: "pgled-legacy-sess", Kind: domain.LedgerArtifact})
	if err != nil || len(rows) != 1 {
		t.Fatalf("imported entries = %+v %v (want exactly one, idempotent)", rows, err)
	}
	got, err := st.Get(ctx, rows[0].ID)
	if err != nil || got == nil || string(got.Body) != "legacy upload" || strings.Join(got.Tags, ",") != "team_a" ||
		got.StepIndex != 1 || got.ToolName != LegacyArtifactToolName {
		t.Fatalf("imported entry = %+v %v", got, err)
	}
	if d := got.ExpiresAt.Sub(now); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("imported entry retention = %v, want the ledger's (1h)", d)
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
