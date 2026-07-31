package config

import (
	"fmt"
	"reflect"
	"testing"
)

// expand must be ORDER-INDEPENDENT. Go randomises map iteration, so the previous
// implementation returned {"a":1} or {"a":{"b":2}} depending on which key the
// runtime happened to visit first — the same input expanding two ways in one
// process, roughly half the time each.
//
// Repeated deliberately: a single pass had a ~50% chance of passing against the
// broken code, which is exactly how the defect survived.
func TestExpand_ScalarWinsRegardlessOfMapOrder(t *testing.T) {
	want := map[string]any{"a": 1}
	for i := 0; i < 200; i++ {
		got := expand(map[string]any{"a": 1, "a.b": 2})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %#v, want %#v — the scalar must win every time, "+
				"not whenever the map iterator happens to cooperate", i, got, want)
		}
	}
}

// The same, deeper: a scalar at any level beats a subtree that would replace it.
func TestExpand_ScalarWinsAtDepth(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := expand(map[string]any{"a.b": 1, "a.b.c": 2})
		want := map[string]any{"a": map[string]any{"b": 1}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %#v, want %#v", i, got, want)
		}
	}
}

// A subtree with no colliding scalar is untouched — the fix must not turn
// "prefer the scalar" into "drop every nested key".
func TestExpand_NestedKeysSurviveWithoutACollision(t *testing.T) {
	got := expand(map[string]any{"a.b": 1, "a.c": 2, "d": 3})
	want := map[string]any{
		"a": map[string]any{"b": 1, "c": 2},
		"d": 3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Stable across runs: the same input expands to the same thing every time, which
// is what lets a caller diff two expansions and believe the difference.
func TestExpand_IsDeterministic(t *testing.T) {
	in := map[string]any{
		"x.y.z": 1, "x.y": 2, "x": 3, "p.q": 4, "p": 5, "solo": 6,
	}
	first := fmt.Sprintf("%#v", expand(in))
	for i := 0; i < 100; i++ {
		if got := fmt.Sprintf("%#v", expand(in)); got != first {
			t.Fatalf("iteration %d differed:\n  %s\n  %s", i, first, got)
		}
	}
}
