package app

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-784: the production builder. Window 0 disables the history; without a
// database it is in memory; over a migrated database a done call written by one
// boot is matched by the next.
func TestBuildActionHistory_PersistsAcrossBoots(t *testing.T) {
	ctx := context.Background()
	if buildActionHistory(ctx, 0, nil) != nil {
		t.Fatal("window 0 must disable the history")
	}
	if buildActionHistory(ctx, 60, nil) == nil {
		t.Fatal("no database: an in-memory history is still built")
	}
	dsn, _ := pgscratch.Migrated(t) // skips without PG_TEST_DSN
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM action_history WHERE scope = 'conversation:appah-conv'`)
	}
	clean()
	t.Cleanup(clean)
	args := []byte(`{"to":"a@x.com","subject":"Weekly report"}`)
	first := buildActionHistory(ctx, 60, pool)
	first.Finish(ctx, first.Begin("conversation:appah-conv", "send_mail", args, domain.ActionRunning), []string{"message_id=m-1"})
	second := buildActionHistory(ctx, 60, pool) // the next boot
	if got := second.Matches(ctx, "conversation:appah-conv", "send_mail", args, domain.ActionRef{}); len(got) != 1 {
		t.Fatalf("the next boot does not see the earlier write: %+v", got)
	}
}
