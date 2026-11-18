package postgres

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1393 against a real migrated Postgres (PG_TEST_DSN). The identity scan port
// applies the substrate's read rules INSIDE: a retired observation is never
// scanned (ADR-0147 D6), owner:alice evidence is invisible to the producers'
// ScopeSystem read (the owner fence) but visible to ScopeMaintenance, and a row in
// another namespace never leaks. Each test uses its own namespaces and deletes
// exactly what it wrote. The file name sorts before migration_guard_test.go on
// purpose (that test drops and recreates the public schema).

func newIdentityScanFixture(t *testing.T) (*PgIdentityScanSource, *pgxpool.Pool, context.Context, string, string) {
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
	ns := fmt.Sprintf("pgtest-idscan-%d", time.Now().UnixNano())
	other := ns + "-other"
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM typed_row_retirements WHERE namespace_id = ANY($1::text[])`,
			`DELETE FROM observations WHERE namespace_id = ANY($1::text[])`,
			`DELETE FROM evidence WHERE namespace_id = ANY($1::text[])`,
		} {
			if _, err := pool.Exec(bg, q, []string{ns, other}); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	return NewPgIdentityScanSource(pool), pool, ctx, ns, other
}

func TestPg_IdentityScan_AliasObservationsApplyRetirementFenceAndNamespace(t *testing.T) {
	scans, pool, ctx, ns, other := newIdentityScanFixture(t)
	const pred = "pgtest_alias"
	evidence := func(namespace, id string, tags []string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
			VALUES ($1,$2,'pgtest',$1,'sha256:test',$3)`, id, namespace, tags); err != nil {
			t.Fatalf("seed evidence %s: %v", id, err)
		}
	}
	observe := func(namespace, entity, value, evidenceID, ref string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO observations (namespace_id, entity_id, predicate, value_type, value_text, occurred_at, evidence_id, source_ref)
			VALUES ($1::text, $2::text, $3::text, 'text', $4::text, now(), $5::text, $6::text)`,
			namespace, entity, pred, value, evidenceID, ref); err != nil {
			t.Fatalf("seed observation %s: %v", entity, err)
		}
	}
	pub, priv, otherEv := ns+"-ev-pub", ns+"-ev-alice", ns+"-ev-other"
	// Untagged evidence is an EMPTY classification, never NULL: the column is
	// NOT NULL DEFAULT '{}' (0011_evidence.sql) and a nil Go slice binds as NULL.
	evidence(ns, pub, []string{})
	evidence(ns, priv, []string{"owner:alice"})
	evidence(other, otherEv, []string{})
	observe(ns, "pgtest/live", "Acme Ltd", pub, ns+"-ref-live")
	observe(ns, "pgtest/gone", "Acme Limited", pub, ns+"-ref-gone")
	observe(ns, "pgtest/private", "Acme Private", priv, ns+"-ref-private")
	observe(other, "pgtest/elsewhere", "Acme Elsewhere", otherEv, ns+"-ref-other")
	if _, err := pool.Exec(ctx, `
		INSERT INTO typed_row_retirements (namespace_id, source_ref, reason, retired_by)
		VALUES ($1::text, $2::text, 'source_deleted', 'pgtest')`, ns, ns+"-ref-gone"); err != nil {
		t.Fatalf("seed retirement: %v", err)
	}

	entities := func(scope *domain.TagPredicate) []string {
		t.Helper()
		rows, err := scans.AliasObservations(ctx, scope, ns, []string{pred}, 0, 50)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.EntityID)
		}
		sort.Strings(out)
		return out
	}
	if got := entities(domain.ScopeSystem); fmt.Sprint(got) != "[pgtest/live]" {
		t.Errorf("ScopeSystem scan = %v; want only the live, unowned, unretired row in this namespace", got)
	}
	if got := entities(domain.ScopeMaintenance); fmt.Sprint(got) != "[pgtest/live pgtest/private]" {
		t.Errorf("ScopeMaintenance scan = %v; want the owner's row too, never the retired one", got)
	}
}

// Every scan parses and runs on a real server (untyped parameters in a row-value
// comparison are where Postgres refuses), and returns nothing for an empty
// namespace.
func TestPg_IdentityScan_EveryQueryRuns(t *testing.T) {
	scans, _, ctx, ns, _ := newIdentityScanFixture(t)
	now := time.Now().UTC()
	pos := domain.IdentityScanPosition{At: now, ID: "x"}
	sc := domain.ScopeSystem
	check := func(name string, n int, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if n != 0 {
			t.Errorf("%s: an empty namespace returned %d rows", name, n)
		}
	}
	a, err := scans.AliasObservations(ctx, sc, ns, []string{"alias"}, 42, 10)
	check("alias observations", len(a), err)
	e, err := scans.EntitiesSince(ctx, sc, ns, pos, 10)
	check("entities since", len(e), err)
	p, err := scans.PartiesForEntity(ctx, sc, ns, "customer/nobody", 8)
	check("parties for entity", len(p), err)
	e, err = scans.EntitiesWithParty(ctx, sc, ns, "customer:nobody", "customer", 8)
	check("entities with party", len(e), err)
	ev, err := scans.EventsSince(ctx, sc, ns, pos, 10)
	check("events since", len(ev), err)
	r, err := scans.RoleEntities(ctx, sc, ns, "no-such-event", 16)
	check("role entities", len(r), err)
	ev, err = scans.PriorEventsSharingEntity(ctx, sc, ns, []string{"shipment/S-1"}, pos, now.Add(-time.Hour), 8)
	check("prior events", len(ev), err)
	l, err := scans.ConfirmedIdentityLinks(ctx, sc, ns, 10)
	check("confirmed links", len(l), err)
	l, err = scans.CandidateLinksSince(ctx, sc, ns, []string{"declared", "reference"}, pos, 10)
	check("candidate links", len(l), err)
	_, ok, err := scans.EntityByID(ctx, sc, ns, "customer/nobody")
	if err != nil || ok {
		t.Errorf("entity by id = %v, %v; want false, nil", ok, err)
	}
	ids, err := scans.EntitiesByLocal(ctx, sc, ns, `%\`, 8)
	check("entities by local (metacharacters are data)", len(ids), err)
}
