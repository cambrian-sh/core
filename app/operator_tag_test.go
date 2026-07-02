package app

import (
	"reflect"
	"testing"
)

// applyTag adds (widen) and removes (narrow) without duplicating. ADR-0047 A1.2/0047-25.
func TestApplyTag(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		tag  string
		add  bool
		want []string
	}{
		{"widen-new", []string{"a"}, "b", true, []string{"a", "b"}},
		{"widen-existing-noop", []string{"a", "b"}, "b", true, []string{"a", "b"}},
		{"narrow-existing", []string{"a", "b"}, "b", false, []string{"a"}},
		{"narrow-absent-noop", []string{"a"}, "z", false, []string{"a"}},
		{"widen-from-empty", nil, "a", true, []string{"a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyTag(c.tags, c.tag, c.add)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("applyTag(%v,%q,%v) = %v, want %v", c.tags, c.tag, c.add, got, c.want)
			}
		})
	}
}

// stringSliceFromMeta coerces []string and JSON-round-tripped []interface{}.
func TestStringSliceFromMeta(t *testing.T) {
	if got := stringSliceFromMeta([]string{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("[]string case: got %v", got)
	}
	if got := stringSliceFromMeta([]interface{}{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("[]interface{} case: got %v", got)
	}
	if got := stringSliceFromMeta(nil); got != nil {
		t.Fatalf("nil case: got %v", got)
	}
	if got := stringSliceFromMeta("not-a-slice"); got != nil {
		t.Fatalf("scalar case: got %v", got)
	}
}
