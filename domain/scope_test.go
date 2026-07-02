package domain

import (
	"reflect"
	"testing"
)

func TestScopeConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		scope   ScopeConfig
		wantErr bool
	}{
		{"empty is valid", ScopeConfig{}, false},
		{"disjoint sets valid", ScopeConfig{RequiredTags: []string{"order_db"}, AnyOfTags: []string{"published", "public_kb"}, ForbiddenTags: []string{"internal_only"}}, false},
		{"required also forbidden", ScopeConfig{RequiredTags: []string{"secrets"}, ForbiddenTags: []string{"secrets"}}, true},
		{"anyof fully denied", ScopeConfig{AnyOfTags: []string{"a", "b"}, ForbiddenTags: []string{"a", "b"}}, true},
		{"anyof partially denied is valid", ScopeConfig{AnyOfTags: []string{"a", "b"}, ForbiddenTags: []string{"a"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.scope.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewEffectiveScope_Union(t *testing.T) {
	caller := ScopeConfig{RequiredTags: []string{"customer_789"}, ForbiddenTags: []string{"secrets"}}
	agent := ScopeConfig{RequiredTags: []string{"order_db"}, ForbiddenTags: []string{"internal_only"}}
	eff := NewEffectiveScope(caller, agent)

	if want := []string{"customer_789", "order_db"}; !reflect.DeepEqual(eff.RequiredTags, want) {
		t.Errorf("RequiredTags = %v, want %v", eff.RequiredTags, want)
	}
	if want := []string{"internal_only", "secrets"}; !reflect.DeepEqual(eff.ForbiddenTags, want) {
		t.Errorf("ForbiddenTags = %v, want %v", eff.ForbiddenTags, want)
	}
}

func TestNewEffectiveScope_AnyOfIsCNFNotUnion(t *testing.T) {
	// Each side's AnyOf becomes its own clause; both must be satisfied.
	caller := ScopeConfig{AnyOfTags: []string{"published"}}
	agent := ScopeConfig{AnyOfTags: []string{"support"}}
	eff := NewEffectiveScope(caller, agent)

	if len(eff.AnyOfClauses) != 2 {
		t.Fatalf("expected 2 CNF clauses, got %d: %v", len(eff.AnyOfClauses), eff.AnyOfClauses)
	}
	// A single side present collapses to one clause (behaves like the old flat list).
	one := NewEffectiveScope(ScopeConfig{AnyOfTags: []string{"published"}}, ScopeConfig{})
	if len(one.AnyOfClauses) != 1 {
		t.Fatalf("expected 1 clause when only one side sets AnyOf, got %d", len(one.AnyOfClauses))
	}
}

// docMatches is a local reference implementation of the three-set/CNF predicate,
// asserting the WORKED EXAMPLE from ADR-0034 D12 holds at the value-object level.
func docMatches(eff EffectiveScope, tags []string) bool {
	if eff.System {
		return true
	}
	has := func(t string) bool {
		for _, x := range tags {
			if x == t {
				return true
			}
		}
		return false
	}
	for _, f := range eff.ForbiddenTags { // ForbiddenTags wins (precedence)
		if has(f) {
			return false
		}
	}
	for _, r := range eff.RequiredTags {
		if !has(r) {
			return false
		}
	}
	for _, clause := range eff.AnyOfClauses { // each clause is an OR; all clauses ANDed
		ok := false
		for _, a := range clause {
			if has(a) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func TestEffectiveScope_D12WorkedExample(t *testing.T) {
	caller := ScopeConfig{RequiredTags: []string{"customer_789"}, AnyOfTags: []string{"published"}, ForbiddenTags: []string{"secrets"}}
	agent := ScopeConfig{AnyOfTags: []string{"support"}, ForbiddenTags: []string{"internal_only"}}
	eff := NewEffectiveScope(caller, agent)

	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"forbidden disqualifies", []string{"customer_789", "published", "internal_only"}, false},
		{"fails caller AnyOf", []string{"customer_789", "support"}, false},
		{"passes both clauses", []string{"customer_789", "published", "support"}, true},
		{"missing required", []string{"published", "support"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := docMatches(eff, tc.tags); got != tc.want {
				t.Errorf("docMatches(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestEffectiveScope_Unsatisfiable(t *testing.T) {
	eff := NewEffectiveScope(
		ScopeConfig{RequiredTags: []string{"secrets"}},
		ScopeConfig{ForbiddenTags: []string{"secrets"}},
	)
	bad, reason := eff.Unsatisfiable()
	if !bad {
		t.Fatalf("expected unsatisfiable, got satisfiable")
	}
	if reason == "" {
		t.Errorf("expected a non-empty reason for the audit log")
	}
}

func TestScopeSystem_BypassesAndIsNotProducedByIntersection(t *testing.T) {
	if !ScopeSystem.System {
		t.Fatalf("ScopeSystem.System must be true")
	}
	eff := NewEffectiveScope(ScopeConfig{ForbiddenTags: []string{"secrets"}}, ScopeConfig{})
	if eff.System {
		t.Fatalf("intersection must never produce a System scope")
	}
}
