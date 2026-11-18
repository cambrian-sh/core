package knowledgecommit

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/objects"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// End to end through the real knowledge_commit path (ADR-0149 D3/D4, P3): an
// agent proposes a case status, a person approves it through the approval hub,
// the bridge commits it into the Postgres knowledge store under the live
// ontology — and the case's object row shows the COMMITTED value, never the
// agent's candidate.
func TestPg_E2E_KnowledgeCommitReachesTheObjectRow(t *testing.T) {
	// pgscratch.Pool skips without PG_TEST_DSN, FAILS on a database whose name does not
	// say test (a skip there hid this test, ENG-2100) and migrates the database to HEAD.
	// Shadowing scratch schema: the global, append-only declarations table starts empty
	// there instead of the test TRUNCATE-ing the migrated one
	// (T-20261114-postgres-adapter-tests-drop-migrated-tables).
	pool, ctx := pgscratch.Pool(t, "kc_objects_test", pgscratch.Options{Shadow: true,
		Migrations: []string{"0040_ontology_declarations.sql", "0041_ontology_drafts.sql"}})
	const ns = "p3test-kc"
	clean := func() {
		bg := context.Background()
		for _, q := range []string{`DELETE FROM resolutions WHERE namespace_id = $1`, `DELETE FROM knowledge_items WHERE namespace_id = $1`} {
			_, _ = pool.Exec(bg, q, ns)
		}
		_, _ = pool.Exec(bg, `DROP TABLE IF EXISTS objects_p3case`)
		_, _ = pool.Exec(bg, `DELETE FROM object_table_builds WHERE table_name = 'objects_p3case'`)
	}
	clean()
	t.Cleanup(clean)
	caseKind := domain.KindSpec{
		Kind: "p3case", Table: true, Policy: domain.ResolutionPolicyDesignatedActor, ResolutionScope: domain.ResolutionScopeEntity,
		Designated: &domain.DesignatedActors{ApprovedViaCommit: true},
		Predicates: map[string]domain.ValueSpec{"status": {Type: "text", Enum: []string{"open", "closed"}}},
	}
	onto, err := ontology.Load(ctx, pool, domain.OntologyDoc{Kinds: []domain.KindSpec{caseKind}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewPgKnowledgeStore(pool, onto)
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Now: func() time.Time { return now }})
	svc, err := New(store, onto, nil, hub, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }

	status := func() any {
		var v *string
		err := pool.QueryRow(ctx, `SELECT status__text FROM `+objects.TableName("p3case")+
			` WHERE namespace_id = $1 AND entity_id = 'p3case/1'`, ns).Scan(&v)
		if err != nil {
			return nil
		}
		if v == nil {
			return "<null>"
		}
		return *v
	}

	res, err := svc.Propose(ctx, scout, alice, Proposal{NamespaceID: ns, Kind: "p3case", EntityID: "p3case/1",
		Values: []domain.StatementValue{{Predicate: "status", Type: "text", Text: "closed"}}}, true, "")
	if err != nil || res.ApprovalID == "" {
		t.Fatalf("propose: %+v %v", res, err)
	}
	if got := status(); got != nil {
		t.Fatalf("an agent's candidate is not the committed state: the object has no row yet, got %v", got)
	}
	now = now.Add(time.Minute)
	if _, err := hub.Answer(ctx, res.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "closed" {
		t.Fatalf("after the person approves, the committed status is the object's current value: %v", got)
	}
}
