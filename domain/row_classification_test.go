package domain

import (
	"errors"
	"reflect"
	"testing"
)

// ADR-0158 D1: the store validates a typed row's classification the way ENG-1041
// validates a mapping spec, and refuses the kernel-stamped namespaces.
func TestNormalizeRowClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      []string
		want    []string
		refused bool
	}{
		{name: "nil is empty", in: nil, want: nil},
		{name: "empty is nil", in: []string{}, want: nil},
		{name: "one tag", in: []string{"hr"}, want: []string{"hr"}},
		{name: "sorted canonically", in: []string{"restricted", "hr", "finance"}, want: []string{"finance", "hr", "restricted"}},
		{name: "party-scoped tag passes", in: []string{"party:customer"}, want: []string{"party:customer"}},
		{name: "blank", in: []string{"hr", ""}, refused: true},
		{name: "whitespace only", in: []string{"  "}, refused: true},
		{name: "leading space", in: []string{" hr"}, refused: true},
		{name: "trailing space", in: []string{"hr\t"}, refused: true},
		{name: "duplicate", in: []string{"hr", "finance", "hr"}, refused: true},
		{name: "owner tag", in: []string{"owner:alice"}, refused: true},
		{name: "bare owner prefix", in: []string{"owner:"}, refused: true},
		{name: "provenance stamp", in: []string{"provenance:source=svc"}, refused: true},
		{name: "ingress surface tag", in: []string{"hr", "ingress:payroll"}, refused: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeRowClassification(c.in)
			if c.refused {
				if !errors.Is(err, ErrRowClassificationRefused) {
					t.Fatalf("want ErrRowClassificationRefused, got %v (%v)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestNormalizeRowClassification_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := []string{"b", "a"}
	if _, err := NormalizeRowClassification(in); err != nil {
		t.Fatal(err)
	}
	if in[0] != "b" || in[1] != "a" {
		t.Fatalf("input mutated: %v", in)
	}
}
