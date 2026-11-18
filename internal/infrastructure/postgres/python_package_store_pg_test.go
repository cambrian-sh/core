package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Per-agent Python package approvals (ADR-0157 D7.8.4) against a real, migrated
// Postgres (PG_TEST_DSN; 0057_python_package_approvals). Rows use the "pgppt-"
// target prefix, removed before and after.
func newPythonPackageStore(t *testing.T) (*PgPythonPackageStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st := NewPgPythonPackageStore(pool)
	if !st.Ready(ctx) {
		t.Skip("python package tables missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		c := context.Background()
		tx, err := pool.Begin(c)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(c) }()
		// python_package_events is append-only by trigger; test cleanup alone
		// disables triggers, for this transaction only.
		_, _ = tx.Exec(c, `SET LOCAL session_replication_role = replica`)
		_, _ = tx.Exec(c, `DELETE FROM python_package_events WHERE target LIKE 'pgppt-%'`)
		_, _ = tx.Exec(c, `DELETE FROM python_package_approvals WHERE target LIKE 'pgppt-%'`)
		_ = tx.Commit(c)
	}
	clean()
	t.Cleanup(clean)
	return st, pool, ctx
}

func TestPg_PythonPackages_OpenDecideReopenAudit(t *testing.T) {
	st, pool, ctx := newPythonPackageStore(t)
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	lock := domain.PythonPackageLock{Index: "offline", Entries: []domain.PythonLockEntry{
		{Name: "polars", Version: "1.9.0", Hashes: []string{strings.Repeat("1", 64)}},
		{Name: "pyarrow", Version: "17.0.0", Hashes: []string{strings.Repeat("3", 64)}},
	}}
	a := domain.PythonPackageApproval{Target: "pgppt-analyst", Name: "Polars", Version: "1.9.0", Extras: []string{"pyarrow"},
		Lock: lock, BaseGeneration: "0123456789abcdef", RequestedBy: "profile:pgppt-analyst", RequestedAt: at}
	rec, created, err := st.Open(ctx, a, "profile:pgppt-analyst")
	if err != nil || !created {
		t.Fatalf("open: %v created=%v", err, created)
	}
	if rec.Name != "polars" || rec.State != domain.PythonPackagePending || rec.LockSHA256 != lock.SHA256() ||
		rec.ID != domain.PythonPackageApprovalID("pgppt-analyst", "polars", "1.9.0", lock.SHA256()) {
		t.Fatalf("record: %+v", rec)
	}
	again, created, err := st.Open(ctx, a, "profile:pgppt-analyst")
	if err != nil || created || again.ID != rec.ID {
		t.Fatalf("a second open of the same lock returns the record: %v %v", err, created)
	}
	if err := st.AttachCard(ctx, rec.ID, "apr-1"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, rec.ID)
	if err != nil || got.ApprovalID != "apr-1" || len(got.Lock.Entries) != 2 || got.Lock.Entries[1].Hashes[0] != strings.Repeat("3", 64) ||
		len(got.Extras) != 1 || got.BaseGeneration != "0123456789abcdef" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := st.Decide(ctx, rec.ID, domain.PythonPackageExpired, "", "card expired", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Reopen(ctx, rec.ID, "apr-2", "profile:pgppt-analyst", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Decide(ctx, rec.ID, domain.PythonPackageApproved, "operator:op-1", "", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Decide(ctx, rec.ID, domain.PythonPackageApproved, "operator:op-1", "", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err) // idempotent: no second audit row
	}
	if err := st.AppendEvent(ctx, domain.PythonPackageEvent{ApprovalID: rec.ID, Target: rec.Target, Name: rec.Name, Version: rec.Version,
		Action: domain.PythonPackageActionInstalled, Actor: "system:kernel", Generation: "fedcba9876543210", At: at.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(ctx, rec.ID)
	if got.State != domain.PythonPackageApproved || got.ApprovalID != "apr-2" || got.DecidedBy != "operator:op-1" {
		t.Fatalf("after reopen + approve: %+v", got)
	}
	evs, err := st.Events(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var acts []string
	for _, e := range evs {
		acts = append(acts, e.Action)
	}
	if strings.Join(acts, ",") != "requested,expired,requested,approved,installed" || evs[4].Generation != "fedcba9876543210" {
		t.Fatalf("audit trail: %v", acts)
	}
	byT, err := st.ForTarget(ctx, "pgppt-analyst")
	if err != nil || len(byT) != 1 {
		t.Fatalf("for target: %v %v", byT, err)
	}
	byP, err := st.ForPackage(ctx, "POLARS", "1.9.0")
	if err != nil || len(byP) != 1 {
		t.Fatalf("for package (normalized): %v %v", byP, err)
	}
	if err := st.Decide(ctx, "ppa-missing", domain.PythonPackageApproved, "x", "", at); !errors.Is(err, domain.ErrPythonPackageNotFound) {
		t.Fatalf("decide unknown: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE python_package_events SET actor = 'x' WHERE record_id = $1`, rec.ID); err == nil {
		t.Fatal("python_package_events must be append-only")
	}
}
