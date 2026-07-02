package memory

import (
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// ADR-0049 D9: a tool verb maps deterministically to the transition it performs.
func TestActionVerb(t *testing.T) {
	cases := map[string]string{
		"write_file":  "write",
		"overwrite":   "write",
		"create_dir":  "write",
		"mkdir":       "write",
		"delete_file": "delete",
		"remove":      "delete",
		"read_file":   "",
		"list_dir":    "",
		"web_search":  "",
	}
	for tool, want := range cases {
		if got := actionVerb(tool); got != want {
			t.Errorf("actionVerb(%q) = %q; want %q", tool, got, want)
		}
	}
}

// ADR-0049 D9 (revised): a delete observes exists=false; a file write OR read observes
// exists=true plus the content baseline ref; a directory has no content_ref.
func TestObservedFields_DrivenByEngagements(t *testing.T) {
	if f := observedFields("file", "delete_file", "cid-x"); f["exists"] != false || f["content_ref"] != nil {
		t.Errorf("delete must observe exists=false and clear content; got %v", f)
	}
	wf := observedFields("file", "write_file", "cid-w")
	if wf["exists"] != true || wf["content_ref"] != "cid-w" {
		t.Errorf("file write must observe exists=true + the content baseline; got %v", wf)
	}
	// A READ is discovery — it observes existence + the content it just saw.
	rf := observedFields("file", "read_file", "cid-r")
	if rf["exists"] != true || rf["content_ref"] != "cid-r" {
		t.Errorf("a read must observe exists=true + the content baseline; got %v", rf)
	}
	// A dir engagement has no content_ref (a directory has no content body).
	if dw := observedFields("dir", "list_dir", "cid-d"); dw["exists"] != true || dw["content_ref"] != nil {
		t.Errorf("dir engagement must set exists with no content_ref; got %v", dw)
	}
}

// ADR-0049 D9: the merge is last-write-wins PER FIELD by ordinal, and untouched fields
// are retained.
func TestMergeObservation_PerFieldLWW(t *testing.T) {
	t0 := time.Now()
	cur := mergeObservation(nil, entityObservation{Seq: 1, At: t0, Fields: map[string]any{"exists": true, "content_ref": "v1"}})
	// A newer overwrite supersedes only content_ref; exists is untouched and retained.
	cur = mergeObservation(cur, entityObservation{Seq: 2, At: t0, Fields: map[string]any{"content_ref": "v2"}})
	if cur["exists"].Value != true {
		t.Errorf("untouched field must be retained; got %v", cur["exists"])
	}
	if cur["content_ref"].Value != "v2" || cur["content_ref"].Seq != 2 {
		t.Errorf("newer observation must win the field; got %v", cur["content_ref"])
	}
	// An OLDER observation (lower ordinal) must NOT clobber a field already owned by a newer one.
	cur = mergeObservation(cur, entityObservation{Seq: 0, At: t0, Fields: map[string]any{"content_ref": "stale"}})
	if cur["content_ref"].Value != "v2" {
		t.Errorf("a stale (lower-ordinal) observation must lose; got %v", cur["content_ref"])
	}
}

// ADR-0049 D9 acceptance: REPLAY-REBUILD EQUALS LIVE. Folding observations live (in
// arrival order) and rebuilding from a shuffled provenance produce the SAME view.
func TestRebuildFields_ReplayEqualsLive(t *testing.T) {
	t0 := time.Now()
	// A realistic life-cycle on one file: write → overwrite → delete.
	obs := []entityObservation{
		{Seq: 1, At: t0, Fields: map[string]any{"exists": true, "content_ref": "c1"}},
		{Seq: 2, At: t0.Add(time.Second), Fields: map[string]any{"content_ref": "c2"}}, // overwrite
		{Seq: 3, At: t0.Add(2 * time.Second), Fields: map[string]any{"exists": false}}, // delete
	}

	// LIVE: fold in arrival order.
	live := map[string]fieldValue{}
	for _, o := range obs {
		live = mergeObservation(live, o)
	}

	// REBUILD: from a shuffled copy of the same provenance.
	shuffled := append([]entityObservation(nil), obs...)
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	rebuilt := rebuildFields(shuffled)

	if !reflect.DeepEqual(live, rebuilt) {
		t.Fatalf("replay-rebuild must equal live\n live=%v\n rebuilt=%v", live, rebuilt)
	}
	// And the end state reflects the actions: deleted, last content_ref retained as history.
	if live["exists"].Value != false {
		t.Errorf("a delete must leave exists=false; got %v", live["exists"])
	}
	if live["content_ref"].Value != "c2" {
		t.Errorf("the last overwrite's content_ref must persist; got %v", live["content_ref"])
	}
}

// materializedValues drops the ordinal bookkeeping for a reader.
func TestMaterializedValues(t *testing.T) {
	fields := map[string]fieldValue{
		"exists":      {Value: false, Seq: 3},
		"content_ref": {Value: "c2", Seq: 2},
	}
	vals := materializedValues(fields)
	if vals["exists"] != false || vals["content_ref"] != "c2" {
		t.Errorf("materialized values must project current field values; got %v", vals)
	}
}

// ADR-0049 §A1.1: the entity-level valid-time is the most recent field observation time
// (the staleness signal ADR-0051 D3 reads). Empty entity ⇒ zero time (maximally stale).
func TestMaterializedObservedAt(t *testing.T) {
	t0 := time.Now()
	fields := map[string]fieldValue{
		"exists":      {Value: true, Seq: 1, At: t0},
		"content_ref": {Value: "c2", Seq: 2, At: t0.Add(5 * time.Second)},
	}
	if got := materializedObservedAt(fields); !got.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("observed-at must be the latest field time; got %v want %v", got, t0.Add(5*time.Second))
	}
	if got := materializedObservedAt(map[string]fieldValue{}); !got.IsZero() {
		t.Errorf("empty entity must be maximally stale (zero time); got %v", got)
	}
}

// ADR-0049 §A1.2: drift is a pre-existing field whose value a winning observation CHANGES.
// First-touch (no prior field) is discovery, not drift; an unchanged re-observation and a
// losing (lower-ordinal) observation are not drift.
func TestDetectFieldDrift(t *testing.T) {
	t0 := time.Now()
	cur := map[string]fieldValue{
		"exists":      {Value: true, Seq: 1, At: t0},
		"content_ref": {Value: "c1", Seq: 1, At: t0},
	}

	// A read that finds content_ref changed → one drift on content_ref (exists unchanged).
	got := detectFieldDrift(cur, entityObservation{Seq: 2, At: t0.Add(time.Second),
		Fields: map[string]any{"exists": true, "content_ref": "c2"}})
	if len(got) != 1 || got[0].Field != "content_ref" || got[0].OldValue != "c1" || got[0].NewValue != "c2" {
		t.Fatalf("expected one content_ref drift c1→c2; got %v", got)
	}

	// Re-observing the SAME values → no drift.
	if d := detectFieldDrift(cur, entityObservation{Seq: 2, At: t0,
		Fields: map[string]any{"exists": true, "content_ref": "c1"}}); len(d) != 0 {
		t.Errorf("unchanged re-observation must not drift; got %v", d)
	}

	// First-touch of a brand-new field is discovery, not drift.
	if d := detectFieldDrift(map[string]fieldValue{}, entityObservation{Seq: 1, At: t0,
		Fields: map[string]any{"exists": true}}); len(d) != 0 {
		t.Errorf("first observation is discovery, not drift; got %v", d)
	}

	// A losing observation (not strictly newer) changes nothing → no drift.
	if d := detectFieldDrift(cur, entityObservation{Seq: 1, At: t0,
		Fields: map[string]any{"content_ref": "stale"}}); len(d) != 0 {
		t.Errorf("a non-winning observation must not drift; got %v", d)
	}

	// Deterministic order: two changed fields come back sorted by name.
	multi := detectFieldDrift(cur, entityObservation{Seq: 3, At: t0,
		Fields: map[string]any{"exists": false, "content_ref": "c9"}})
	if len(multi) != 2 || multi[0].Field != "content_ref" || multi[1].Field != "exists" {
		t.Errorf("drift must be sorted by field; got %v", multi)
	}
}
