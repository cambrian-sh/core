package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0137 S7 / D8 (ENG-411 stage C) against real Postgres: a participant erases its
// own evidence-keyed rows inside the erase transaction (so a plugin row referencing the
// evidence no longer rolls the erase back), its counts reach the receipt, the observer
// is told once after commit (never for a replay), a failing participant rolls the whole
// erase back, and receipts list newest first with a cursor.

type lctestParticipant struct{ fail bool }

func (lctestParticipant) Name() string { return "lctest" }

func (p lctestParticipant) EraseEvidence(ctx context.Context, tx domain.ErasureTx, ids []string) (map[string]int, error) {
	if p.fail {
		return nil, errors.New("participant refused")
	}
	n, err := tx.Exec(ctx, `DELETE FROM lctest_premium_ref WHERE evidence_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	return map[string]int{"lctest_premium_ref": int(n)}, nil
}

type lctestObserver struct{ seen []domain.ErasureReceipt }

func (o *lctestObserver) ObserveErasure(_ context.Context, r domain.ErasureReceipt) {
	o.seen = append(o.seen, r)
}

func TestPg_Erase_ParticipantObserverAndListing(t *testing.T) {
	a, ks, er0, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-p1", "agent:a", erasureSecret, "dg-p1", 1)
	seedErasureEvidence(t, a, ctx, "lctest-p1")
	seedLifecycleDoc(t, a, ks, ctx, "lctest-p2", "agent:a", "p2", "dg-p2", 1)
	seedErasureEvidence(t, a, ctx, "lctest-p2")
	execSQL(t, a, ctx, `CREATE TABLE lctest_premium_ref (evidence_id TEXT REFERENCES evidence(id))`)
	execSQL(t, a, ctx, `INSERT INTO lctest_premium_ref VALUES ('lctest-p1-ev'), ('lctest-p2-ev')`)

	// A failing participant rolls the erase back: nothing is gone, no receipt.
	failing := NewPgErasureStore(er0.l, WithErasureParticipants(lctestParticipant{fail: true}))
	if _, err := failing.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-p1"}})); err == nil {
		t.Fatal("a failing participant must fail the erase")
	}
	if countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = 'lctest-p1'`) != 1 ||
		countRows(t, a, ctx, `SELECT count(*) FROM memory_erasures WHERE principal = 'lctest-op'`) != 0 {
		t.Fatal("a failed participant left a partial erase")
	}

	obs := &lctestObserver{}
	er := NewPgErasureStore(er0.l, WithErasureParticipants(lctestParticipant{}), WithErasureObserver(obs))
	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-p1"}}))
	if err != nil {
		t.Fatalf("Erase with a participant for the referencing rows: %v", err)
	}
	if rec.Counts["lctest_premium_ref"] != 1 || rec.Counts[eraseEvidence] != 1 {
		t.Fatalf("counts = %v, want the participant's row and the evidence", rec.Counts)
	}
	if countRows(t, a, ctx, `SELECT count(*) FROM lctest_premium_ref WHERE evidence_id = 'lctest-p1-ev'`) != 0 ||
		countRows(t, a, ctx, `SELECT count(*) FROM lctest_premium_ref WHERE evidence_id = 'lctest-p2-ev'`) != 1 {
		t.Fatal("the participant must delete exactly the erased evidence's rows")
	}
	if _, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-p1"}})); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(obs.seen) != 1 || obs.seen[0].RequestID != rec.RequestID {
		t.Fatalf("observer saw %d receipts, want exactly the executed one", len(obs.seen))
	}

	rec2, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-p2"}}))
	if err != nil {
		t.Fatal(err)
	}
	all, err := er.ListReceipts(ctx, domain.ErasureReceiptFilter{Principal: "lctest-op"})
	if err != nil || len(all) != 2 {
		t.Fatalf("ListReceipts = %d, %v; want 2", len(all), err)
	}
	// Newest first, and the cursor pages past the first.
	if all[0].RequestID != rec2.RequestID && !all[0].ErasedAt.Equal(all[1].ErasedAt) {
		t.Fatalf("not newest first: %s then %s", all[0].RequestID, all[1].RequestID)
	}
	rest, err := er.ListReceipts(ctx, domain.ErasureReceiptFilter{Principal: "lctest-op", Before: all[0].RequestID})
	if err != nil || len(rest) != 1 || rest[0].RequestID != all[1].RequestID {
		t.Fatalf("page after %s = %+v, %v", all[0].RequestID, rest, err)
	}
	if none, err := er.ListReceipts(ctx, domain.ErasureReceiptFilter{Principal: "lctest-nobody"}); err != nil || len(none) != 0 {
		t.Fatalf("another principal's listing = %d, %v", len(none), err)
	}
}
