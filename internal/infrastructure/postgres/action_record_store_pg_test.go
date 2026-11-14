package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The durable action record (ADR-0154) against a real, migrated Postgres
// (PG_TEST_DSN; 0046_action_records). Rows use the "pgact-" key prefix.
func newActionRecordStore(t *testing.T) (*PgActionRecordStore, context.Context) {
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
	st := NewPgActionRecordStore(pool)
	if !st.Ready(ctx) {
		t.Skip("action_records missing (run `orchestrator migrate up` first)")
	}
	clean := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM action_records WHERE key LIKE 'pgact-%'`) }
	clean()
	t.Cleanup(clean)
	return st, ctx
}

func TestPg_ActionRecordLifecycleAndIdempotentRecord(t *testing.T) {
	st, ctx := newActionRecordStore(t)
	rec, created, err := st.Record(ctx, domain.DurableAction{Key: "pgact-1", Tool: "erp.create_order", RunID: "run-1", StepIndex: 2, Principal: "ayse", ArgsHash: "h"})
	if err != nil || !created || rec.State != domain.ActionStateProposed || rec.Principal != "ayse" {
		t.Fatalf("Record = %+v created=%v err=%v", rec, created, err)
	}
	if again, created, err := st.Record(ctx, domain.DurableAction{Key: "pgact-1", Tool: "other"}); err != nil || created || again.Tool != "erp.create_order" {
		t.Fatalf("second Record = %+v created=%v err=%v", again, created, err)
	}
	if _, err := st.Advance(ctx, "pgact-1", domain.ActionStateAccepted, domain.ActionUpdate{}); !errors.Is(err, domain.ErrActionTransition) {
		t.Fatalf("proposed → accepted: %v", err)
	}
	if _, err := st.Advance(ctx, "pgact-1", domain.ActionStateDispatched, domain.ActionUpdate{Error: "timeout"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Advance(ctx, "pgact-1", domain.ActionStateAccepted, domain.ActionUpdate{Result: &domain.ToolCallResponse{ResultJSON: []byte(`{"id":"SO-7"}`)}})
	if err != nil || got.State != domain.ActionStateAccepted || got.Result == nil || string(got.Result.ResultJSON) != `{"id":"SO-7"}` {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	if _, err := st.Get(ctx, "pgact-nope"); !errors.Is(err, domain.ErrActionRecordNotFound) {
		t.Fatalf("Get(unknown) = %v", err)
	}
}

// The ADR-0145 D9 idempotency cache over the Postgres record survives a restart.
func TestPg_DurableIdempotencyAcrossRestart(t *testing.T) {
	st, ctx := newActionRecordStore(t)
	meta := domain.DurableAction{Key: "pgact-" + domain.IdempotencyKey("run-9", 0, "erp.post", []byte(`{}`)), Tool: "erp.post", RunID: "run-9"}
	_, _, finish := domain.NewDurableIdempotencyCache(time.Hour, st).BeginAction(ctx, meta)
	finish(domain.ToolCallResponse{ResultJSON: []byte(`"ok"`)}, true)
	stored, found, _ := domain.NewDurableIdempotencyCache(time.Hour, st).BeginAction(ctx, meta)
	if !found || string(stored.ResultJSON) != `"ok"` {
		t.Fatalf("retry after restart: found=%v %s", found, stored.ResultJSON)
	}
}
