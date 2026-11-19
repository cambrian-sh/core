package memory

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func supRow(id string, score float64, tags ...string) domain.SearchResult {
	meta := map[string]interface{}{}
	if len(tags) > 0 {
		meta["tags"] = tags
	}
	return domain.SearchResult{Document: domain.Document{ID: id, Metadata: meta}, Score: score, RawScore: score}
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The copies of one chunk across a document's revisions score alike; the in-force
// copy must lead and the superseded copies must fall below other answers.
func TestSupersededDemotion_InForceCopyLeadsAndOtherAnswersReturn(t *testing.T) {
	var q QueryService
	q.EnableSupersededDemotion([]string{"Superseded"}, 0.5) // case-insensitive marker
	in := []domain.SearchResult{
		supRow("rev1", 0.80, "sop-001", "rev1", "superseded"),
		supRow("rev2", 0.80, "sop-001", "rev2", "superseded"),
		supRow("rev3", 0.80, "sop-001", "rev3", "SUPERSEDED"),
		supRow("rev4", 0.80, "sop-001", "rev4", "in-force"),
		supRow("note", 0.60),
	}
	got := ids(q.superseded.demote(in))
	want := []string{"rev4", "note", "rev1", "rev2", "rev3"}
	if !sameIDs(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A superseded copy that scored far above its successor's identical copy (lexical
// tie-break by rank, per-chunk graph signals) still lands strictly below it.
func TestSupersededDemotion_CopyStaysBelowSuccessorsSameText(t *testing.T) {
	var q QueryService
	q.EnableSupersededDemotion([]string{"superseded"}, 0.5)
	text := "- Site Manager: owns the receiving dock schedule."
	old := supRow("rev1", 0.40, "superseded")
	old.Document.Text = text
	cur := supRow("rev4", 0.15, "in-force")
	cur.Document.Text = "  - site manager:   owns the receiving dock schedule. " // same text, other formatting
	other := supRow("other", 0.10)
	other.Document.Text = "Something else."
	got := ids(q.superseded.demote([]domain.SearchResult{old, other, cur}))
	if !sameIDs(got, []string{"rev4", "other", "rev1"}) {
		t.Fatalf("order = %v, want rev4 other rev1 (0.15, 0.10, 0.5×min(0.40,0.15)=0.075)", got)
	}
}

// Demoted, never dropped; RawScore (what the relevance floor tests) is untouched.
func TestSupersededDemotion_KeepsRowsAndRawScore(t *testing.T) {
	var q QueryService
	q.EnableSupersededDemotion([]string{"superseded"}, 0.5)
	out := q.superseded.demote([]domain.SearchResult{supRow("old", 0.9, "superseded"), supRow("new", 0.3)})
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	for _, r := range out {
		if r.Document.ID == "old" && (r.Score != 0.45 || r.RawScore != 0.9) {
			t.Errorf("old: score %.3f raw %.3f, want 0.45 / 0.9", r.Score, r.RawScore)
		}
	}
	// An old revision that alone matches the query strongly still outranks a weak
	// in-force hit: an explicit ask still finds it.
	if out[0].Document.ID != "old" {
		t.Errorf("order = %v; the strongly matching old revision should still lead a weak hit", ids(out))
	}
}

// A pool that is all superseded (a read filter that selects old revisions) keeps the
// order it had: every row is scaled alike.
func TestSupersededDemotion_AllSupersededPoolKeepsOrder(t *testing.T) {
	var q QueryService
	q.EnableSupersededDemotion([]string{"superseded"}, 0.5)
	got := ids(q.superseded.demote([]domain.SearchResult{
		supRow("a", 0.9, "superseded"), supRow("b", 0.7, "superseded"), supRow("c", 0.2, "superseded"),
	}))
	if !sameIDs(got, []string{"a", "b", "c"}) {
		t.Fatalf("order = %v, want a b c", got)
	}
}

func TestSupersededDemotion_OffLeavesRankingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []string
		factor  float64
	}{
		{"no markers", nil, 0.5},
		{"blank markers", []string{" ", ""}, 0.5},
		{"factor 1", []string{"superseded"}, 1},
		{"factor 0", []string{"superseded"}, 0},
		{"factor above 1", []string{"superseded"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var q QueryService
			q.EnableSupersededDemotion(tc.markers, tc.factor)
			if q.superseded.active() {
				t.Fatal("demotion active; want off")
			}
			in := []domain.SearchResult{supRow("old", 0.9, "superseded"), supRow("new", 0.5)}
			out := q.superseded.demote(in)
			if out[0].Document.ID != "old" || out[0].Score != 0.9 {
				t.Errorf("ranking changed while off: %v score %.2f", ids(out), out[0].Score)
			}
		})
	}
}
