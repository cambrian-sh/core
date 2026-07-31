package postgres

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The SQL mirror must agree with domain.SessionIsolation.Allows, which is the
// authoritative form. These assert the SHAPE of the predicate; the semantics are
// pinned by the domain tests.
func TestIsolationExpressions_NarrowsToTheSession(t *testing.T) {
	got := render(t, isolationExpressions(domain.IsolateTo("sess-A"), ""))
	if !strings.Contains(got, "session_id") {
		t.Fatalf("predicate does not mention session_id: %s", got)
	}
	if !strings.Contains(got, "sess-A") {
		t.Fatalf("predicate does not bind the session: %s", got)
	}
	// Unowned material must survive, or isolation becomes a store reset.
	if !strings.Contains(strings.ToUpper(got), "IS NULL") {
		t.Fatalf("unowned corpus is not admitted: %s", got)
	}
}

func TestIsolationExpressions_ExcludeUnowned(t *testing.T) {
	got := render(t, isolationExpressions(&domain.SessionIsolation{SessionID: "sess-A"}, ""))
	if strings.Contains(strings.ToUpper(got), "IS NULL") {
		t.Fatalf("IncludeUnowned=false still admitted unowned material: %s", got)
	}
}

// A nil or bypass predicate adds NO SQL — the fail-closed decision belongs to the
// chokepoint, not to a second copy down here.
func TestIsolationExpressions_NilAndBypassAddNothing(t *testing.T) {
	if got := isolationExpressions(nil, "c"); got != nil {
		t.Fatalf("nil produced a predicate: %v", got)
	}
	if got := isolationExpressions(domain.IsolationBypass(), "c"); got != nil {
		t.Fatalf("bypass produced a predicate: %v", got)
	}
}

// The alias must qualify the column, or a JOINed query leaves `metadata`
// ambiguous and the query fails at runtime rather than in a test.
func TestIsolationExpressions_AliasQualifiesTheColumn(t *testing.T) {
	got := render(t, isolationExpressions(domain.IsolateTo("sess-A"), "c"))
	if !strings.Contains(got, "c.metadata") {
		t.Fatalf("alias not applied: %s", got)
	}
}

// Isolation and the tag predicate must stay SEPARATE. If a session id ever shows
// up inside the tag containment predicate, the two have been conflated and both
// became unauditable.
func TestIsolationIsNotATagPredicate(t *testing.T) {
	tags := render(t, scopeExpressionsOn(&domain.TagPredicate{RequiredTags: []string{"sales"}}, ""))
	if strings.Contains(tags, "session_id") {
		t.Fatalf("the tag predicate is matching on session_id: %s", tags)
	}
	iso := render(t, isolationExpressions(domain.IsolateTo("sess-A"), ""))
	if strings.Contains(iso, "tags") {
		t.Fatalf("the isolation predicate is matching on tags: %s", iso)
	}
}
