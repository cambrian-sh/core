package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type formsReader struct {
	persons map[string]bool
	aliases map[string]string // alias → id
	err     error
}

func (r formsReader) GetPrincipal(_ context.Context, id string) (Principal, error) {
	if r.persons[id] {
		return Principal{ID: id}, nil
	}
	return Principal{}, ErrPrincipalNotFound
}

func (r formsReader) AliasesOf(_ context.Context, id string) ([]string, error) {
	var out []string
	for a, p := range r.aliases {
		if p == id {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r formsReader) Canonical(_ context.Context, ref string) (string, bool, error) {
	if r.err != nil {
		return "", false, r.err
	}
	if id, ok := r.aliases[ref]; ok {
		return id, true, nil
	}
	if len(ref) > 5 && ref[:5] == "user:" && r.persons[ref[5:]] {
		return ref[5:], true, nil
	}
	return "", false, nil
}

func TestPrincipalForms(t *testing.T) {
	ctx := context.Background()
	reg := formsReader{persons: map[string]bool{"alice": true}, aliases: map[string]string{"agent:alice": "alice"}}
	all := []string{"alice", "user:alice", "agent:alice"}
	for ref, want := range map[string][]string{
		"alice":       all,
		"user:alice":  {"user:alice", "alice", "agent:alice"},
		"agent:alice": {"agent:alice", "alice", "user:alice"},
		"agent:bot":   {"agent:bot"}, // not a person: exact only
		"carol":       {"carol"},     // unknown bare id
	} {
		if got := PrincipalForms(ctx, reg, ref); !reflect.DeepEqual(got, want) {
			t.Errorf("PrincipalForms(%q) = %v, want %v", ref, got, want)
		}
	}
	if got := PrincipalForms(ctx, nil, "alice"); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("nil registry: %v", got)
	}
	broken := formsReader{err: errors.New("down")}
	if got := PrincipalForms(ctx, broken, "alice"); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("a registry error must not widen the filter: %v", got)
	}
}
