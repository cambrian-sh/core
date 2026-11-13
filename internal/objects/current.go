// Package objects holds the operational object model's current state (ADR-0149
// D3, D4, D8): one generated objects_<kind> table per kind declared with
// Table, one row per object, one typed column per declared property.
//
// History (observations, knowledge items and their resolutions) stays the only
// truth. The tables are a derived, rebuildable view, and this package is their
// only writer:
//
//   - Current (this file) is the ONE current-value function: pure, it picks
//     each property's value from an object's candidates;
//   - PlanTables / Generate (tables.go) are the ONE generator: DDL from the
//     live ontology, run by internal/ontology at boot and on every applied
//     change;
//   - ApplyStateTx (state.go) is the ONE maintainer, called inside the
//     transaction of every write that changes "current"; Rebuild recomputes a
//     whole table through the same function, so rebuild equals incremental.
//
// Reading the tables is ADR-0149 P4: Reader.List (the objects query, D6) and
// Reader.Get (GetObject, D7) — validate.go, plan.go, token.go (pure), exec.go
// and get.go (I/O).
package objects

import (
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Candidate is one value history offers for one property of one object: a
// non-retired observation, or a value of a knowledge item that is a current
// resolution winner.
type Candidate struct {
	Predicate string
	Value     domain.StatementValue
	// At is the event time: an observation's occurred_at, an item's asserted_at.
	At time.Time
	// Source is who said it: an observation's evidence source_id ("" without
	// evidence), an item's asserted_by. Per-property Authority ranks it.
	Source string
	// ID breaks a tie at equal rank and time (the larger wins); unique per row:
	// "o" + the zero-padded observation id, "i" + the item id.
	ID string
	// Tags and Parties are the visibility of the row the value came from, the
	// per-property companions the P4 fence reads. Tags is nil when the row has
	// no evidence to classify it (an observation without evidence), which no
	// filtered reader may see.
	Tags       []string
	Parties    []string
	EvidenceID string
}

// Property is one property's current value: the winning candidate, and whether
// another source currently says something else (ADR-0149 D4: the losing values
// stay in history and the conflict is flagged).
type Property struct {
	Candidate
	Conflict bool
}

// State is an object's current state under its kind's declaration.
type State struct {
	// Props holds the declared properties that have a value of the declared type.
	Props map[string]Property
	// Extra holds undeclared properties of an OPEN kind (the table's extra jsonb).
	Extra map[string]Property
}

// Current computes an object's state from its candidates (ADR-0149 D4). Pure,
// and independent of the candidates' order.
//
// For each property:
//   - a declared property considers only values of its declared type; a value
//     of another type stays in history, is not projected, and is reported by
//     the ontology's violations report;
//   - an undeclared property is kept (in Extra) only when the kind is open;
//   - the winner is the best by (authority rank, event time, id): with no
//     Authority every source ranks the same, so the latest by event time wins
//     and the id breaks a tie; with one, a listed source beats an unlisted one
//     and an earlier-listed source a later one;
//   - Conflict is set when another source's best value differs from the winner's.
func Current(spec domain.KindSpec, cands []Candidate) State {
	byPred := map[string][]Candidate{}
	for _, c := range cands {
		byPred[c.Predicate] = append(byPred[c.Predicate], c)
	}
	st := State{Props: map[string]Property{}, Extra: map[string]Property{}}
	for pred, cs := range byPred {
		vs, declared := spec.Predicates[pred]
		switch {
		case declared:
			var typed []Candidate
			for _, c := range cs {
				if c.Value.Type == vs.Type {
					typed = append(typed, c)
				}
			}
			if p, ok := pick(typed, vs.Authority); ok {
				st.Props[pred] = p
			}
		case spec.Open:
			if p, ok := pick(cs, nil); ok {
				st.Extra[pred] = p
			}
		}
	}
	return st
}

// pick chooses the winner of one property's candidates and its conflict flag.
func pick(cs []Candidate, authority []string) (Property, bool) {
	if len(cs) == 0 {
		return Property{}, false
	}
	best := BestBySource(cs, authority)
	sources := make([]string, 0, len(best))
	for s := range best {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	better := betterUnder(authority)
	var win Candidate
	for i, s := range sources {
		if c := best[s]; i == 0 || better(c, win) {
			win = c
		}
	}
	p := Property{Candidate: win}
	for _, s := range sources {
		if s != win.Source && !SameValue(best[s].Value, win.Value) {
			p.Conflict = true
		}
	}
	return p, true
}

// BestBySource is each source's best candidate among cs (one property's
// candidates) under authority — the per-source values a conflict is judged
// between. GetObject shows the other sources' ones as alternatives.
func BestBySource(cs []Candidate, authority []string) map[string]Candidate {
	better := betterUnder(authority)
	out := map[string]Candidate{}
	for _, c := range cs {
		if cur, ok := out[c.Source]; !ok || better(c, cur) {
			out[c.Source] = c
		}
	}
	return out
}

// betterUnder orders candidates by (authority rank, event time, id).
func betterUnder(authority []string) func(a, b Candidate) bool {
	rank := func(c Candidate) int {
		for i, s := range authority {
			if s == c.Source {
				return i
			}
		}
		return len(authority)
	}
	return func(a, b Candidate) bool {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return a.ID > b.ID
	}
}

// SameValue reports whether two typed values are equal (the predicate name is
// not part of the value).
func SameValue(a, b domain.StatementValue) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case "date":
		return a.Date.Equal(b.Date)
	case "number":
		return a.Number == b.Number
	case "entity":
		return a.EntityRef == b.EntityRef
	default:
		return a.Text == b.Text
	}
}
