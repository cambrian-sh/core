package objects

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
)

var t0 = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func text(pred, v string) domain.StatementValue {
	return domain.StatementValue{Predicate: pred, Type: "text", Text: v}
}

func cand(pred string, v domain.StatementValue, at time.Duration, src, id string) Candidate {
	v.Predicate = pred
	return Candidate{Predicate: pred, Value: v, At: t0.Add(at), Source: src, ID: id, Tags: []string{}, Parties: []string{}}
}

func spec(open bool, preds map[string]domain.ValueSpec) domain.KindSpec {
	return domain.KindSpec{Kind: "order", Table: true, Open: open, Predicates: preds}
}

func TestCurrent_LatestByEventTimeWins(t *testing.T) {
	s := spec(false, map[string]domain.ValueSpec{"status": {Type: "text"}})
	st := Current(s, []Candidate{
		cand("status", text("status", "shipped"), 2*time.Hour, "erp", "o2"),
		cand("status", text("status", "open"), time.Hour, "erp", "o9"), // arrived later, happened earlier
	})
	if got := st.Props["status"]; got.Value.Text != "shipped" || got.Conflict {
		t.Fatalf("latest by event time, one source, no conflict: %+v", got)
	}
}

func TestCurrent_TieBreaksOnID(t *testing.T) {
	s := spec(false, map[string]domain.ValueSpec{"status": {Type: "text"}})
	st := Current(s, []Candidate{
		cand("status", text("status", "a"), 0, "erp", "o1"),
		cand("status", text("status", "b"), 0, "erp", "o2"),
	})
	if st.Props["status"].Value.Text != "b" {
		t.Fatalf("equal time: the larger id wins, got %+v", st.Props["status"])
	}
}

func TestCurrent_AuthorityRanksBeforeTime(t *testing.T) {
	s := spec(false, map[string]domain.ValueSpec{"status": {Type: "text", Authority: []string{"erp", "crm"}}})
	cs := []Candidate{
		cand("status", text("status", "picking"), 1*time.Hour, "erp", "o1"),
		cand("status", text("status", "cancelled"), 3*time.Hour, "user:alice", "iA"),
		cand("status", text("status", "shipped"), 2*time.Hour, "crm", "o2"),
	}
	got := Current(s, cs).Props["status"]
	if got.Value.Text != "picking" || got.Source != "erp" || !got.Conflict {
		t.Fatalf("ERP wins for status even when older, and the disagreement is flagged: %+v", got)
	}
	// Without ERP, crm (listed) still beats the newer unlisted person.
	got = Current(s, cs[1:]).Props["status"]
	if got.Value.Text != "shipped" || got.Source != "crm" {
		t.Fatalf("a listed source beats an unlisted one: %+v", got)
	}
}

func TestCurrent_ConflictOnlyAcrossSources(t *testing.T) {
	s := spec(false, map[string]domain.ValueSpec{"status": {Type: "text"}})
	same := Current(s, []Candidate{
		cand("status", text("status", "open"), 0, "erp", "o1"),
		cand("status", text("status", "shipped"), time.Hour, "erp", "o2"),
		cand("status", text("status", "shipped"), 30*time.Minute, "user:bob", "iB"),
	}).Props["status"]
	if same.Value.Text != "shipped" || same.Conflict {
		t.Fatalf("an older value of the SAME source is history, not a conflict; agreeing sources are no conflict: %+v", same)
	}
	diff := Current(s, []Candidate{
		cand("status", text("status", "shipped"), time.Hour, "erp", "o2"),
		cand("status", text("status", "lost"), 30*time.Minute, "user:bob", "iB"),
	}).Props["status"]
	if !diff.Conflict {
		t.Fatalf("another source currently saying something else is a conflict: %+v", diff)
	}
}

func TestCurrent_TypeMismatchIsNotProjected(t *testing.T) {
	s := spec(false, map[string]domain.ValueSpec{"total": {Type: "number"}})
	st := Current(s, []Candidate{
		cand("total", domain.StatementValue{Type: "number", Number: 10}, 0, "erp", "o1"),
		cand("total", text("total", "ten"), time.Hour, "erp", "o2"), // newer, wrong type
	})
	if got := st.Props["total"]; got.Value.Type != "number" || got.Value.Number != 10 {
		t.Fatalf("a mismatched value stays in history and is not projected: %+v", got)
	}
	only := Current(s, []Candidate{cand("total", text("total", "ten"), 0, "erp", "o1")})
	if _, ok := only.Props["total"]; ok || len(only.Extra) != 0 {
		t.Fatalf("only a mismatched value: no property, not in extra: %+v", only)
	}
}

func TestCurrent_StrictDropsOpenKeepsUndeclared(t *testing.T) {
	preds := map[string]domain.ValueSpec{"status": {Type: "text"}}
	cs := []Candidate{cand("colour", text("colour", "red"), 0, "erp", "o1")}
	if st := Current(spec(false, preds), cs); len(st.Extra) != 0 || len(st.Props) != 0 {
		t.Fatalf("strict: an undeclared predicate is not projected: %+v", st)
	}
	if st := Current(spec(true, preds), cs); st.Extra["colour"].Value.Text != "red" {
		t.Fatalf("open: an undeclared predicate lands in extra: %+v", st)
	}
}

// --- properties (pgregory.net/rapid) ---

var genSources = []string{"", "erp", "crm", "user:alice", "agent:x"}

func genCandidates(t *rapid.T) []Candidate {
	n := rapid.IntRange(0, 25).Draw(t, "n")
	out := make([]Candidate, n)
	for i := range out {
		pred := rapid.SampledFrom([]string{"status", "total", "colour"}).Draw(t, "pred")
		var v domain.StatementValue
		switch rapid.IntRange(0, 2).Draw(t, "type") {
		case 0:
			v = text(pred, rapid.SampledFrom([]string{"open", "shipped", "lost"}).Draw(t, "text"))
		case 1:
			v = domain.StatementValue{Predicate: pred, Type: "number", Number: float64(rapid.IntRange(0, 3).Draw(t, "num"))}
		default:
			v = domain.StatementValue{Predicate: pred, Type: "date", Date: t0.Add(time.Duration(rapid.IntRange(0, 3).Draw(t, "d")) * time.Hour)}
		}
		out[i] = cand(pred, v, time.Duration(rapid.IntRange(0, 4).Draw(t, "at"))*time.Minute,
			rapid.SampledFrom(genSources).Draw(t, "src"), fmt.Sprintf("o%03d", i))
	}
	return out
}

func genSpec(t *rapid.T) domain.KindSpec {
	auth := rapid.SliceOfDistinct(rapid.SampledFrom(genSources[1:]), func(s string) string { return s }).Draw(t, "auth")
	if rapid.Bool().Draw(t, "noauth") {
		auth = nil
	}
	return spec(rapid.Bool().Draw(t, "open"), map[string]domain.ValueSpec{
		"status": {Type: "text", Authority: auth},
		"total":  {Type: "number"},
	})
}

// Current is a function of the candidate SET: order never matters.
func TestCurrent_Property_OrderIndependent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, cs := genSpec(t), genCandidates(t)
		perm := rapid.Permutation(cs).Draw(t, "perm")
		if a, b := Current(s, cs), Current(s, perm); !reflect.DeepEqual(a, b) {
			t.Fatalf("order changed the state:\n%+v\n%+v", a, b)
		}
	})
}

// The winner is a candidate of the declared type, no candidate of its property
// beats it, and the conflict flag is exactly "another source's best differs".
func TestCurrent_Property_WinnerIsTheBestAndConflictIsExact(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, cs := genSpec(t), genCandidates(t)
		st := Current(s, cs)
		for pred, vs := range s.Predicates {
			var typed []Candidate
			for _, c := range cs {
				if c.Predicate == pred && c.Value.Type == vs.Type {
					typed = append(typed, c)
				}
			}
			p, ok := st.Props[pred]
			if len(typed) == 0 {
				if ok {
					t.Fatalf("%s: projected with no typed candidate: %+v", pred, p)
				}
				continue
			}
			if !ok {
				t.Fatalf("%s: typed candidates but no value", pred)
			}
			rank := func(c Candidate) int {
				for i, a := range vs.Authority {
					if a == c.Source {
						return i
					}
				}
				return len(vs.Authority)
			}
			found := false
			for _, c := range typed {
				if c.ID == p.ID {
					found = true
				}
				if rank(c) < rank(p.Candidate) ||
					(rank(c) == rank(p.Candidate) && c.At.After(p.At)) ||
					(rank(c) == rank(p.Candidate) && c.At.Equal(p.At) && c.ID > p.ID) {
					t.Fatalf("%s: %+v beats the winner %+v", pred, c, p)
				}
			}
			if !found {
				t.Fatalf("%s: winner %s is not a candidate", pred, p.ID)
			}
			// Brute-force conflict: the best per other source differs.
			want := false
			for _, src := range genSources {
				if src == p.Source {
					continue
				}
				var best *Candidate
				for i := range typed {
					c := typed[i]
					if c.Source != src {
						continue
					}
					if best == nil || rank(c) < rank(*best) || (rank(c) == rank(*best) && (c.At.After(best.At) || (c.At.Equal(best.At) && c.ID > best.ID))) {
						best = &c
					}
				}
				if best != nil && !SameValue(best.Value, p.Value) {
					want = true
				}
			}
			if p.Conflict != want {
				t.Fatalf("%s: conflict %v, brute force %v", pred, p.Conflict, want)
			}
		}
		if !s.Open && len(st.Extra) != 0 {
			t.Fatalf("strict kind with extra: %+v", st.Extra)
		}
		if s.Open {
			_, has := st.Extra["colour"]
			any := false
			for _, c := range cs {
				any = any || c.Predicate == "colour"
			}
			if has != any {
				t.Fatalf("open kind: colour in extra %v, candidates %v", has, any)
			}
		}
	})
}

// Adding a candidate that cannot win and agrees with nothing new never changes
// the winner: a duplicate of the current winner with a smaller id.
func TestCurrent_Property_DuplicateOfWinnerIsInert(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, cs := genSpec(t), genCandidates(t)
		st := Current(s, cs)
		p, ok := st.Props["status"]
		if !ok {
			return
		}
		dup := p.Candidate
		dup.ID = "a" // sorts below every generated id
		after := Current(s, append(append([]Candidate(nil), cs...), dup))
		if !reflect.DeepEqual(after.Props["status"], p) {
			t.Fatalf("an inert duplicate changed the winner: %+v -> %+v", p, after.Props["status"])
		}
	})
}
