package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1393: the identity producers' scan port applies the substrate's read rules
// itself. These run without a database: every method must refuse a nil predicate
// (fail closed, ErrQueryScopeMissing) and an empty namespace before it touches
// the pool. The rules against real rows are in identity_scan_source_pg_test.go.
func TestIdentityScanSource_RefusesNilScopeAndEmptyNamespace(t *testing.T) {
	s := NewPgIdentityScanSource(nil)
	ctx := context.Background()
	pos := domain.IdentityScanPosition{}
	calls := map[string]func(scope *domain.TagPredicate, ns string) error{
		"AliasObservations": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.AliasObservations(ctx, sc, ns, []string{"alias"}, 0, 10)
			return err
		},
		"EntitiesSince": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.EntitiesSince(ctx, sc, ns, pos, 10)
			return err
		},
		"PartiesForEntity": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.PartiesForEntity(ctx, sc, ns, "customer/1", 10)
			return err
		},
		"EntitiesWithParty": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.EntitiesWithParty(ctx, sc, ns, "p", "customer", 10)
			return err
		},
		"EventsSince": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.EventsSince(ctx, sc, ns, pos, 10)
			return err
		},
		"RoleEntities": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.RoleEntities(ctx, sc, ns, "evt", 10)
			return err
		},
		"PriorEventsSharingEntity": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.PriorEventsSharingEntity(ctx, sc, ns, []string{"a/1"}, pos, time.Time{}, 10)
			return err
		},
		"ConfirmedIdentityLinks": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.ConfirmedIdentityLinks(ctx, sc, ns, 10)
			return err
		},
		"CandidateLinksSince": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.CandidateLinksSince(ctx, sc, ns, []string{"declared"}, pos, 10)
			return err
		},
		"EntityExists": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.EntityExists(ctx, sc, ns, "a/1")
			return err
		},
		"EntitiesByLocal": func(sc *domain.TagPredicate, ns string) error {
			_, err := s.EntitiesByLocal(ctx, sc, ns, "1", 10)
			return err
		},
	}
	for name, call := range calls {
		if err := call(nil, "default"); !errors.Is(err, domain.ErrQueryScopeMissing) {
			t.Errorf("%s with a nil scope: err = %v, want ErrQueryScopeMissing", name, err)
		}
		if err := call(domain.ScopeSystem, " "); !errors.Is(err, errIdentityScanNamespace) {
			t.Errorf("%s with no namespace: err = %v, want the namespace refusal", name, err)
		}
	}
}

// A local id is data inside LIKE ... ESCAPE '\': the escape character itself must
// be doubled, or a trailing backslash escapes the next pattern character (the
// premium scanner this port replaced mapped `\` to `\`).
func TestEscapeLikeLiteral_EscapesTheEscapeCharacter(t *testing.T) {
	cases := map[string]string{
		`OPS_412`: `OPS\_412`,
		`100%`:    `100\%`,
		`a\b`:     `a\\b`,
		`plain`:   `plain`,
	}
	for in, want := range cases {
		if got := escapeLikeLiteral(in); got != want {
			t.Errorf("escapeLikeLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}
