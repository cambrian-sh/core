package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrKindRefused marks a validation refusal by a kind declaration — a
// PERMANENT condition. Consumers use it to tell "this write can never succeed"
// (complete with a log; the evidence stays reprocessable) from a transient
// failure (retry). Collapsing the two turns one bad value into an outbox item
// that retries forever.
var ErrKindRefused = errors.New("refused by kind declaration")

// ValueSpec constrains one predicate's value (ADR-0110 D1; memo §5 — the base
// type is domain-agnostic, the spec carries the constraint).
type ValueSpec struct {
	// Type: "date" | "number" | "text" | "entity".
	Type string
	// Min/Max bound a number value inclusively. nil = unbounded on that side;
	// ignored for non-number types.
	Min *float64
	Max *float64
}

// KindSpec declares one knowledge kind's shape: which predicates it may carry,
// with what constraints, resolved under which policy. Specs are DATA — the
// kernel never learns what a predicate means, only what shape it must have.
type KindSpec struct {
	Kind string
	// Policy names the ResolutionAuthority deriving this kind's belief.
	// Empty = ResolutionPolicyLatestAssertion.
	Policy string
	// Predicates is the allowlist. An item of a DECLARED kind carrying an
	// undeclared predicate is refused — "cannot express safely", never a
	// silent drop (a dropped value is invisible data loss).
	Predicates map[string]ValueSpec
}

// ResolutionAuthority derives a belief from the FULL candidate set — a pure
// function, order-independent, holding no state (memo §13; ADR-0110 D3).
// latest_assertion is the built-in implementation of the interface it
// previously WAS.
type ResolutionAuthority interface {
	Policy() string
	Resolve(items []KnowledgeItem) (winner *KnowledgeItem, reason string)
}

// LatestAssertionAuthority is the built-in resolution policy (ADR-0106),
// re-expressed through the extracted interface.
type LatestAssertionAuthority struct{}

func (LatestAssertionAuthority) Policy() string { return ResolutionPolicyLatestAssertion }
func (LatestAssertionAuthority) Resolve(items []KnowledgeItem) (*KnowledgeItem, string) {
	return ResolveLatestAssertion(items)
}

// KindRegistry validates writes against declared kinds and resolves policies
// to authorities. Immutable after construction (post-construction safety: a
// registry that can gain kinds mid-flight can disagree with itself between
// two writes of one batch).
type KindRegistry struct {
	kinds       map[string]KindSpec
	authorities map[string]ResolutionAuthority
}

// NewKindRegistry builds the registry. Refuses duplicate kind declarations
// (two plugins claiming one kind is a fight the boot must referee, not the
// write path), a declared policy nobody registered, and malformed specs — all
// at STARTUP, the chunker-registry rule: unknown route is a boot error, never
// a silent fallback.
func NewKindRegistry(specs []KindSpec, authorities []ResolutionAuthority) (*KindRegistry, error) {
	auth := map[string]ResolutionAuthority{
		ResolutionPolicyLatestAssertion: LatestAssertionAuthority{},
	}
	for _, a := range authorities {
		if a == nil || a.Policy() == "" {
			return nil, fmt.Errorf("kind registry: an authority must name its policy")
		}
		if _, dup := auth[a.Policy()]; dup && a.Policy() != ResolutionPolicyLatestAssertion {
			return nil, fmt.Errorf("kind registry: duplicate authority for policy %q", a.Policy())
		}
		auth[a.Policy()] = a
	}
	kinds := make(map[string]KindSpec, len(specs))
	for _, s := range specs {
		if s.Kind == "" || len(s.Predicates) == 0 {
			return nil, fmt.Errorf("kind registry: a spec needs a kind and at least one predicate")
		}
		if _, dup := kinds[s.Kind]; dup {
			return nil, fmt.Errorf("kind registry: kind %q declared twice", s.Kind)
		}
		for p, v := range s.Predicates {
			switch v.Type {
			case "date", "number", "text", "entity":
			default:
				return nil, fmt.Errorf("kind registry: %s/%s: unknown value type %q", s.Kind, p, v.Type)
			}
		}
		policy := s.Policy
		if policy == "" {
			policy = ResolutionPolicyLatestAssertion
		}
		if _, ok := auth[policy]; !ok {
			return nil, fmt.Errorf("kind registry: kind %q names policy %q but no authority is registered for it", s.Kind, policy)
		}
		s.Policy = policy
		kinds[s.Kind] = s
	}
	return &KindRegistry{kinds: kinds, authorities: auth}, nil
}

// Spec returns the declaration for a kind, if any.
func (r *KindRegistry) Spec(kind string) (KindSpec, bool) {
	if r == nil {
		return KindSpec{}, false
	}
	s, ok := r.kinds[kind]
	return s, ok
}

// Authority returns the resolver for a policy. The default registry always
// carries latest_assertion.
func (r *KindRegistry) Authority(policy string) (ResolutionAuthority, bool) {
	if r == nil {
		if policy == ResolutionPolicyLatestAssertion {
			return LatestAssertionAuthority{}, true
		}
		return nil, false
	}
	a, ok := r.authorities[policy]
	return a, ok
}

// ValidateValues checks typed values against a kind's declaration. A nil
// registry or an UNDECLARED kind passes — adoption is monotonic (ADR-0110 D2)
// and that asymmetry is deliberate: existing producers keep working until they
// declare.
func (r *KindRegistry) ValidateValues(kind string, values []StatementValue) error {
	if r == nil {
		return nil
	}
	spec, ok := r.kinds[kind]
	if !ok {
		return nil
	}
	for _, v := range values {
		vs, declared := spec.Predicates[v.Predicate]
		if !declared {
			return fmt.Errorf("kind %q does not declare predicate %q (declared: %s) — cannot express safely: %w",
				kind, v.Predicate, strings.Join(declaredPredicates(spec), ", "), ErrKindRefused)
		}
		if v.Type != vs.Type {
			return fmt.Errorf("kind %q predicate %q: value type %q, declared %q: %w", kind, v.Predicate, v.Type, vs.Type, ErrKindRefused)
		}
		if vs.Type == "number" {
			if vs.Min != nil && v.Number < *vs.Min {
				return fmt.Errorf("kind %q predicate %q: %g below the declared minimum %g: %w", kind, v.Predicate, v.Number, *vs.Min, ErrKindRefused)
			}
			if vs.Max != nil && v.Number > *vs.Max {
				return fmt.Errorf("kind %q predicate %q: %g above the declared maximum %g: %w", kind, v.Predicate, v.Number, *vs.Max, ErrKindRefused)
			}
		}
	}
	return nil
}

func declaredPredicates(s KindSpec) []string {
	out := make([]string, 0, len(s.Predicates))
	for p := range s.Predicates {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
