package postgres

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-833: current and contradictions put the entity, actor, scope fence and
// LIMIT into SQL instead of loading a whole kind and filtering it in Go. These
// pin the rendered SQL; the behaviour against Postgres is covered by the
// query_belief_selectors / knowledge_case / fence_* Pg suites.

func TestCurrentResolutionsWhere_NarrowsAndFencesInSQL(t *testing.T) {
	a := &sqlArgs{}
	scope := &domain.TagPredicate{RequiredTags: []string{"finance"}}
	where := currentResolutionsWhere(a, "invoice", "latest_assertion", []string{"customer/C-1"}, "erp", scope)
	for _, want := range []string{
		"r.namespace_id=$1", "r.kind=$2", "r.policy=$3",
		"r.system_to IS NULL", "r.item_id IS NOT NULL",
		"r.entity_id = $4", "r.actor = $5",
		"ki.id IS NOT NULL", "ki.classification",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("WHERE lacks %q:\n%s", want, where)
		}
	}
	if a.args[0] != defaultNamespace || a.args[3] != "customer/C-1" || a.args[4] != "erp" {
		t.Errorf("args = %v", a.args)
	}

	// Several entities bind as one array; Maintenance renders no fence at all.
	a = &sqlArgs{}
	where = currentResolutionsWhere(a, "invoice", "latest_assertion", []string{"e1", "e2"}, "", domain.ScopeMaintenance)
	if !strings.Contains(where, "r.entity_id = ANY($4)") || strings.Contains(where, "classification") {
		t.Errorf("multi-entity maintenance WHERE:\n%s", where)
	}
	// A Bypass scope still renders the owner fence (it holds under Bypass).
	a = &sqlArgs{}
	where = currentResolutionsWhere(a, "invoice", "latest_assertion", nil, "", domain.ScopeSystem)
	if !strings.Contains(where, "ki.classification") || strings.Contains(where, "r.entity_id") {
		t.Errorf("bypass WHERE must keep the owner fence and no entity filter:\n%s", where)
	}
}

func TestContradictionKeysSQL_GroupsFencedSidesWithALimit(t *testing.T) {
	a := &sqlArgs{}
	scope := &domain.TagPredicate{RequiredTags: []string{"finance"}}
	q := contradictionKeysSQL(a, "invoice", "latest_assertion", "customer/C-1", "due_date", "erp", scope, 25)
	for _, want := range []string{
		"WITH sides AS (",
		"JOIN statement_values sv ON sv.item_id = r.item_id",
		"ki.classification", // the fence is applied to every side, before grouping
		"sv.predicate = $",
		"GROUP BY entity_id, predicate",
		"count(DISTINCT val) >= 2",
		"bool_or(actor = $",
		"LIMIT $",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("contradiction SQL lacks %q:\n%s", want, q)
		}
	}
	if got := a.args[len(a.args)-1]; got != 25 {
		t.Errorf("last arg (LIMIT) = %v, want 25", got)
	}
	// The fence sits inside the CTE, i.e. before the GROUP BY.
	if strings.Index(q, "ki.classification") > strings.Index(q, "GROUP BY") {
		t.Errorf("fence rendered after grouping:\n%s", q)
	}
}

func TestInNamespace_PinsTypedRowReadsToTheShapesNamespace(t *testing.T) {
	a := &sqlArgs{}
	if got := a.inNamespace("o.namespace_id"); got != " AND o.namespace_id = $1" || a.args[0] != defaultNamespace {
		t.Fatalf("inNamespace = %q %v", got, a.args)
	}
}
