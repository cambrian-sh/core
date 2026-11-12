package authz

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0146 acceptance tests 1 and 2 on the ledger read path. The predicate has
// the shape premium's resolveParts builds for an agent reading for owner A whose
// floor has a positive term (run 20261112T132056Z: owner vantis-ops-lkovacs, floor
// any_of [provenance:source=<A>, shared:vantis-medical, engineering]); the entries
// have the shape every entry of that run had. EnforcingLedger has no own-rows
// logic: it gets the rule from TagPredicate.Allows (AGENTS.md §2.5).

const lkovacs = "vantis-ops-lkovacs"

func ownRowsWorkerScope() *domain.TagPredicate {
	agent := []string{"provenance:source=composed_worker", "ops"}
	return &domain.TagPredicate{
		AnyOfClauses: [][]string{agent, {"provenance:source=" + lkovacs, "shared:vantis-medical", "engineering"}},
		Owners:       []string{lkovacs},
		OwnRows:      &domain.TagPredicate{AnyOfClauses: [][]string{agent}, Owners: []string{lkovacs}},
	}
}

func ownRowsLedger() *EnforcingLedger {
	own := []string{"provenance:source=composed_worker", domain.OwnerTag(lkovacs)}
	return NewEnforcingLedger(&memLedger{rows: []domain.LedgerEntry{
		{ID: "led_step2", RunID: "81394c0ea8e7263a", TurnID: "t1", ConversationID: "sched:sched-7f129a91ec3dece4",
			StepIndex: 2, Tags: own, Body: []byte("weekly summary")},
		{ID: "led_mixed", RunID: "81394c0ea8e7263a", TurnID: "t1", ConversationID: "sched:sched-7f129a91ec3dece4",
			Tags: []string{"provenance:source=composed_worker", domain.OwnerTag(lkovacs), domain.OwnerTag("bob")}},
		{ID: "led_bob", RunID: "81394c0ea8e7263a", TurnID: "t1", ConversationID: "sched:sched-7f129a91ec3dece4",
			Tags: []string{"provenance:source=composed_worker", domain.OwnerTag("bob")}},
	}}, nil)
}

func TestEnforcingLedger_OwnRows_WorkerReadsItsOwnersEntries(t *testing.T) {
	l := ownRowsLedger()
	ctx := domain.WithScope(context.Background(), ownRowsWorkerScope())
	if e, err := l.Get(ctx, "led_step2"); err != nil || e == nil {
		t.Fatalf("ledger_get of the upstream step's entry: %+v %v (the T-20261112 regression)", e, err)
	}
	for name, q := range map[string]domain.LedgerQuery{
		"scope run":          {RunID: "81394c0ea8e7263a", StepIndex: 2, HasStep: true},
		"scope turn":         {TurnID: "t1", Text: "weekly summary"},
		"scope conversation": {ConversationID: "sched:sched-7f129a91ec3dece4"},
	} {
		rows, err := l.Find(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != "led_step2" {
			t.Errorf("ledger_find %s: %v, want only led_step2", name, rows)
		}
	}
	for _, id := range []string{"led_mixed", "led_bob"} {
		if e, _ := l.Get(ctx, id); e != nil {
			t.Errorf("%s readable: mixed or foreign owners are never own rows", id)
		}
	}
}

func TestEnforcingLedger_OwnRows_NoOwnerOnCtxDenied(t *testing.T) {
	l := ownRowsLedger()
	// What the decision point resolves for the same worker with no conversation
	// owner: its floor alone, no Owners, no OwnRows.
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{
		AnyOfClauses: [][]string{{"provenance:source=composed_worker", "ops"}},
	})
	if e, err := l.Get(ctx, "led_step2"); err != nil || e != nil {
		t.Fatalf("an agent acting for nobody read the owner's entry: %+v %v", e, err)
	}
}
