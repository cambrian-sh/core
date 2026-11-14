package postgres

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// latestRevision is the one predicate event lists and counts use to keep the latest
// revision of an edited occurrence (ENG-1026).
func TestLatestRevision_PredicateNamesTheEvidenceChain(t *testing.T) {
	got := latestRevision("ev")
	if !strings.Contains(got, "nv.revises_id = ev.evidence_id") || !strings.HasPrefix(got, " AND NOT EXISTS") {
		t.Fatalf("latestRevision(ev) = %q", got)
	}
}

// ENG-1026 (owner decision 2026-11-14): an order event edited three times appears ONCE
// in the event listing and the catalog event count — its latest revision — while
// every revision row stays stored (history keeps all).
func TestPg_EditedEventListedOnceAtItsLatestRevision(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	prev := ""
	for i, rev := range []string{"1", "2", "3"} {
		ev := "ownleak-ev-edit-" + rev
		var revises any
		if prev != "" {
			revises = prev
		}
		exec(`INSERT INTO evidence (id, namespace_id, source_id, source_key, source_revision, content_hash, classification, revises_id)
			VALUES ($1,'default','ownleak-src-edit','order-7',$2,'sha256:test','{mail}',$3)`, ev, rev, revises)
		exec(`INSERT INTO events (id, namespace_id, event_type, occurred_at, evidence_id, source_ref)
			VALUES ($1,'default','order_placed',now() - make_interval(mins => $2), $3, $4)`,
			"ownleak-evt-edit-"+rev, 10-i, ev, "ownleak-edit-order-7#"+rev)
		exec(`INSERT INTO event_roles (event_id, role, entity_id) VALUES ($1,'order',$2)`, "ownleak-evt-edit-"+rev, slShared)
		prev = ev
	}
	defer exec(`DELETE FROM events WHERE id LIKE 'ownleak-evt-edit-%'`)

	cat := f.plane.Catalog(domain.ScopeSystem)
	page, err := cat.Events(f.ctx, domain.EventListFilter{Namespace: "default", SourceRefPrefix: "ownleak-edit-", Limit: 50})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].SourceRef != "ownleak-edit-order-7#3" {
		t.Fatalf("an event edited 3 times must list once at revision 3, got %+v", page.Events)
	}
	var stored int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM events WHERE id LIKE 'ownleak-evt-edit-%'`).Scan(&stored); err != nil || stored != 3 {
		t.Fatalf("every revision row must stay stored (history keeps all): %d, %v", stored, err)
	}
}
