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
	// Enum, when non-empty, is the closed set of values a TEXT predicate may
	// take (case lifecycle states, a category). A value outside it is refused
	// naming the allowed set. Only valid on "text" — the registry refuses an enum
	// on any other type at boot.
	Enum []string `json:"enum,omitempty"`
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

	// The fields below are the case-modelling extensions
	// (T-20261107-knowledge-substrate-extensions-for-case). Every one is optional
	// and DATA: a kind that sets none of them validates and resolves exactly as
	// before they existed.

	// ResolutionScope selects what one resolution ranges over:
	// ResolutionScopeActor (default, "") resolves per (namespace, kind, entity,
	// actor) — one belief per speaker; ResolutionScopeEntity ALSO resolves per
	// (namespace, kind, entity) across EVERY actor's items (actor column
	// ResolutionActorAll), so an authority can pick "the word of whoever is
	// designated" while the others keep speaking.
	ResolutionScope string `json:"resolution_scope,omitempty"`
	// Designated configures the designated_actor authority (required when
	// Policy is ResolutionPolicyDesignatedActor, refused otherwise).
	Designated *DesignatedActors `json:"designated,omitempty"`
	// Lifecycle constrains one state predicate: its initial states, its allowed
	// transitions and the predicates each state requires. Checked at PutItem
	// against the key's current resolution, inside the resolution-key lock.
	Lifecycle *KindLifecycle `json:"lifecycle,omitempty"`
	// EventTypes declares the event types this kind's lifecycle records, with
	// their roles. A declared type is validated at RecordEvent; an undeclared
	// type passes (monotonic adoption, ADR-0110 D2).
	EventTypes []EventTypeSpec `json:"event_types,omitempty"`
	// CommitClass is the approval action class a knowledge_commit of this kind
	// is routed under, so a deployment's approvals config can name who commits
	// it. "" = ApprovalKindKnowledgeCommit.
	CommitClass string `json:"commit_class,omitempty"`
}

// Resolution scopes (KindSpec.ResolutionScope).
const (
	ResolutionScopeActor  = "actor"
	ResolutionScopeEntity = "entity"
)

// ResolutionActorAll is the resolutions.actor value of an ENTITY-scoped
// resolution row: the belief ranges over every actor's items. A real actor may
// not be named "*" in an entity-scoped kind (PutItem refuses it).
const ResolutionActorAll = "*"

// ResolutionPolicyDesignatedActor names DesignatedActorAuthority.
const ResolutionPolicyDesignatedActor = "designated_actor"

// Reason codes of DesignatedActorAuthority.
const (
	ReasonDesignatedAssertion = "designated_assertion"
	// ReasonNoDesignated: nobody eligible has spoken — no resolution exists (the
	// state stays uncommitted). The store closes the key rather than version it.
	ReasonNoDesignated = "no_designated_assertion"
)

// DesignatedActors is the eligible set of the designated_actor authority. An
// item is eligible when ANY clause admits it.
//
// Roles are deliberately absent: an authority is a pure function over items
// (ADR-0110 D3) and cannot ask a directory who holds a role. Role- and
// relationship-based eligibility is expressed where it can be checked — the
// approval policy that gates knowledge_commit (ADR-0142) — and admitted here by
// ApprovedViaCommit.
type DesignatedActors struct {
	// Principals are actor ids ("user:alice", "agent:x") whose assertions win.
	// The plugin seam refuses an item asserted by one of them
	// (authz.DesignatedActorGuard), so a plugin cannot forge a designated
	// principal's word; only kernel paths write them.
	Principals []string `json:"principals,omitempty"`
	// Sources are in-process source ids (a system of record: "erp") whose
	// assertions win. A producer asserting as a source is trusted in-process
	// code — the same trust the link mechanism ceiling places in a producer's
	// declared mechanism.
	Sources []string `json:"sources,omitempty"`
	// ApprovedViaCommit admits items the knowledge_commit approval handler wrote
	// (SourceRef carries KnowledgeCommitSourceRefPrefix), whoever approved them:
	// the approval policy already decided who may.
	ApprovedViaCommit bool `json:"approved_via_commit,omitempty"`
}

// Eligible reports whether item is admitted by the designated set.
func (d *DesignatedActors) Eligible(item KnowledgeItem) bool {
	if d == nil {
		return false
	}
	if d.ApprovedViaCommit && IsKnowledgeCommitRef(item.SourceRef) {
		return true
	}
	return containsString(d.Principals, item.AssertedBy) || containsString(d.Sources, item.AssertedBy)
}

// KindLifecycle constrains one designated state predicate of a kind.
type KindLifecycle struct {
	// StatePredicate names the state predicate; it must be a declared text
	// predicate.
	StatePredicate string `json:"state_predicate"`
	// Initial lists the states a key may start in (no current state yet).
	// Empty = any state.
	Initial []string `json:"initial,omitempty"`
	// Transitions maps a state to the states it may move to. Staying in the
	// same state is always allowed (editing other fields of a case). A state
	// with no entry is terminal.
	Transitions map[string][]string `json:"transitions,omitempty"`
	// Required maps a state to the predicates that must have a value — on the
	// written item, or carried by the key's current resolved item — before the
	// state is reached.
	Required map[string][]string `json:"required,omitempty"`
}

// EventTypeSpec declares one event type a kind's lifecycle records.
type EventTypeSpec struct {
	Type string `json:"type"`
	// Roles is the allowlist of participant roles. Empty = any role.
	Roles []string `json:"roles,omitempty"`
	// RequiredRoles must each be present at least once.
	RequiredRoles []string `json:"required_roles,omitempty"`
}

// KindAwareAuthority is a ResolutionAuthority whose resolution depends on the
// kind's declaration (its eligible set). The store calls ResolveKind instead of
// Resolve when the authority implements it. Still pure: the spec is data.
type KindAwareAuthority interface {
	ResolutionAuthority
	ResolveKind(spec KindSpec, items []KnowledgeItem) (winner *KnowledgeItem, reason string)
}

// DesignatedActorAuthority resolves to the LATEST assertion among the items the
// kind's DesignatedActors admits (a designated negation retracts). Everyone
// else's items are kept and not resolved over — contradictions are preserved,
// never resolved away. No eligible item ⇒ no resolution (ReasonNoDesignated).
type DesignatedActorAuthority struct{}

func (DesignatedActorAuthority) Policy() string { return ResolutionPolicyDesignatedActor }

// Resolve without a spec admits nobody (fail closed): the eligible set is kind data.
func (DesignatedActorAuthority) Resolve([]KnowledgeItem) (*KnowledgeItem, string) {
	return nil, ReasonNoDesignated
}

// ResolveKind implements KindAwareAuthority.
func (DesignatedActorAuthority) ResolveKind(spec KindSpec, items []KnowledgeItem) (*KnowledgeItem, string) {
	var eligible []KnowledgeItem
	for _, it := range items {
		if spec.Designated.Eligible(it) {
			eligible = append(eligible, it)
		}
	}
	if len(eligible) == 0 {
		return nil, ReasonNoDesignated
	}
	w, reason := ResolveLatestAssertion(eligible)
	if reason == ReasonLatestAssertion {
		reason = ReasonDesignatedAssertion
	}
	return w, reason
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
	// eventTypes indexes declared event types (KindSpec.EventTypes).
	eventTypes map[string]EventTypeSpec
}

// NewKindRegistry builds the registry. Refuses duplicate kind declarations
// (two plugins claiming one kind is a fight the boot must referee, not the
// write path), a declared policy nobody registered, and malformed specs — all
// at STARTUP, the chunker-registry rule: unknown route is a boot error, never
// a silent fallback.
func NewKindRegistry(specs []KindSpec, authorities []ResolutionAuthority) (*KindRegistry, error) {
	auth := map[string]ResolutionAuthority{
		ResolutionPolicyLatestAssertion: LatestAssertionAuthority{},
		ResolutionPolicyDesignatedActor: DesignatedActorAuthority{},
	}
	for _, a := range authorities {
		if a == nil || a.Policy() == "" {
			return nil, fmt.Errorf("kind registry: an authority must name its policy")
		}
		if _, dup := auth[a.Policy()]; dup && a.Policy() != ResolutionPolicyLatestAssertion &&
			a.Policy() != ResolutionPolicyDesignatedActor {
			return nil, fmt.Errorf("kind registry: duplicate authority for policy %q", a.Policy())
		}
		auth[a.Policy()] = a
	}
	kinds := make(map[string]KindSpec, len(specs))
	eventTypes := map[string]EventTypeSpec{}
	eventOwner := map[string]string{}
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
			if len(v.Enum) > 0 && v.Type != "text" {
				return nil, fmt.Errorf("kind registry: %s/%s: an enum is only valid on a text predicate, not %q", s.Kind, p, v.Type)
			}
		}
		policy := s.Policy
		if policy == "" {
			policy = ResolutionPolicyLatestAssertion
		}
		a, ok := auth[policy]
		if !ok {
			return nil, fmt.Errorf("kind registry: kind %q names policy %q but no authority is registered for it", s.Kind, policy)
		}
		s.Policy = policy
		if err := validateCaseExtensions(s, a); err != nil {
			return nil, err
		}
		for _, et := range s.EventTypes {
			if et.Type == "" {
				return nil, fmt.Errorf("kind registry: kind %q declares an event type with no name", s.Kind)
			}
			if prev, dup := eventOwner[et.Type]; dup {
				return nil, fmt.Errorf("kind registry: event type %q declared by both %q and %q", et.Type, prev, s.Kind)
			}
			if len(et.Roles) > 0 {
				for _, rr := range et.RequiredRoles {
					if !containsString(et.Roles, rr) {
						return nil, fmt.Errorf("kind registry: event type %q requires role %q, which its roles (%s) do not allow",
							et.Type, rr, strings.Join(et.Roles, ", "))
					}
				}
			}
			eventOwner[et.Type] = s.Kind
			eventTypes[et.Type] = et
		}
		kinds[s.Kind] = s
	}
	return &KindRegistry{kinds: kinds, authorities: auth, eventTypes: eventTypes}, nil
}

// validateCaseExtensions checks the optional case-modelling fields of one spec
// at BOOT, so a malformed lifecycle is a refused boot rather than a write path
// that refuses everything (or nothing).
func validateCaseExtensions(s KindSpec, a ResolutionAuthority) error {
	switch s.ResolutionScope {
	case "", ResolutionScopeActor:
	case ResolutionScopeEntity:
		// The per-actor rows of an entity-scoped kind are kept under
		// latest_assertion (each speaker's current word, so `contradictions`
		// still shows the disagreement); an entity row under that same policy
		// would read as one more speaker.
		if s.Policy == ResolutionPolicyLatestAssertion {
			return fmt.Errorf("kind registry: kind %q: resolution_scope %q needs a policy other than %q "+
				"(its per-actor rows already resolve under it)", s.Kind, ResolutionScopeEntity, ResolutionPolicyLatestAssertion)
		}
		if _, input := a.(InputResolutionAuthority); input {
			return fmt.Errorf("kind registry: kind %q: resolution_scope %q is not supported for the input-taking policy %q",
				s.Kind, ResolutionScopeEntity, s.Policy)
		}
	default:
		return fmt.Errorf("kind registry: kind %q: unknown resolution_scope %q (want %s|%s)",
			s.Kind, s.ResolutionScope, ResolutionScopeActor, ResolutionScopeEntity)
	}
	if s.Policy == ResolutionPolicyDesignatedActor {
		d := s.Designated
		if d == nil || (len(d.Principals) == 0 && len(d.Sources) == 0 && !d.ApprovedViaCommit) {
			return fmt.Errorf("kind registry: kind %q: policy %q needs a designated set (principals, sources or approved_via_commit)",
				s.Kind, ResolutionPolicyDesignatedActor)
		}
		for _, p := range append(append([]string(nil), d.Principals...), d.Sources...) {
			if p == "" || p == ResolutionActorAll {
				return fmt.Errorf("kind registry: kind %q: designated actor %q is not a valid actor id", s.Kind, p)
			}
		}
	} else if s.Designated != nil {
		return fmt.Errorf("kind registry: kind %q declares a designated set but its policy is %q, not %q",
			s.Kind, s.Policy, ResolutionPolicyDesignatedActor)
	}
	lc := s.Lifecycle
	if lc == nil {
		return nil
	}
	sp, ok := s.Predicates[lc.StatePredicate]
	if !ok || sp.Type != "text" {
		return fmt.Errorf("kind registry: kind %q: lifecycle state predicate %q must be a declared text predicate",
			s.Kind, lc.StatePredicate)
	}
	state := func(where, st string) error {
		if len(sp.Enum) > 0 && !containsString(sp.Enum, st) {
			return fmt.Errorf("kind registry: kind %q: lifecycle %s names state %q, which is not in %s's enum (%s)",
				s.Kind, where, st, lc.StatePredicate, strings.Join(sp.Enum, ", "))
		}
		return nil
	}
	for _, st := range lc.Initial {
		if err := state("initial", st); err != nil {
			return err
		}
	}
	for from, tos := range lc.Transitions {
		if err := state("transitions", from); err != nil {
			return err
		}
		for _, to := range tos {
			if err := state("transitions", to); err != nil {
				return err
			}
		}
	}
	for st, preds := range lc.Required {
		if err := state("required", st); err != nil {
			return err
		}
		for _, p := range preds {
			if _, ok := s.Predicates[p]; !ok {
				return fmt.Errorf("kind registry: kind %q: state %q requires predicate %q, which the kind does not declare",
					s.Kind, st, p)
			}
		}
	}
	return nil
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// EntityScoped reports whether kind also resolves across actors (ResolutionScopeEntity).
func (r *KindRegistry) EntityScoped(kind string) bool {
	s, ok := r.Spec(kind)
	return ok && s.ResolutionScope == ResolutionScopeEntity
}

// HasLifecycle reports whether kind declares a lifecycle (the store reads the
// current resolution only then, so other kinds pay nothing).
func (r *KindRegistry) HasLifecycle(kind string) bool {
	s, ok := r.Spec(kind)
	return ok && s.Lifecycle != nil
}

// ValidateTransition checks a write of next against the key's CURRENT resolved
// item (nil = no current state) under the kind's lifecycle. A kind without a
// lifecycle, an undeclared kind, a negation, and an item that does not carry the
// state predicate all pass. Refusals name the constraint and wrap ErrKindRefused.
func (r *KindRegistry) ValidateTransition(kind string, current *KnowledgeItem, next KnowledgeItem) error {
	spec, ok := r.Spec(kind)
	if !ok || spec.Lifecycle == nil || next.Negation {
		return nil
	}
	lc := spec.Lifecycle
	to, carries := textValue(next.Values, lc.StatePredicate)
	if !carries {
		return nil
	}
	from := ""
	if current != nil {
		from, _ = textValue(current.Values, lc.StatePredicate)
	}
	switch {
	case from == "":
		if len(lc.Initial) > 0 && !containsString(lc.Initial, to) {
			return fmt.Errorf("kind %q lifecycle: %q is not an initial state of %s (initial: %s): %w",
				kind, to, lc.StatePredicate, strings.Join(lc.Initial, ", "), ErrKindRefused)
		}
	case from != to:
		allowed := lc.Transitions[from]
		if !containsString(allowed, to) {
			if len(allowed) == 0 {
				return fmt.Errorf("kind %q lifecycle: transition %s %q -> %q is not declared (%q is terminal): %w",
					kind, lc.StatePredicate, from, to, from, ErrKindRefused)
			}
			return fmt.Errorf("kind %q lifecycle: transition %s %q -> %q is not declared (from %q: %s): %w",
				kind, lc.StatePredicate, from, to, from, strings.Join(allowed, ", "), ErrKindRefused)
		}
	}
	for _, p := range lc.Required[to] {
		if hasValue(next.Values, p) || (current != nil && hasValue(current.Values, p)) {
			continue
		}
		return fmt.Errorf("kind %q lifecycle: state %s=%q requires predicate %q, which has no current value: %w",
			kind, lc.StatePredicate, to, p, ErrKindRefused)
	}
	return nil
}

// ValidateEvent checks an event against its type's declaration. An undeclared
// type, or a nil registry, passes (monotonic adoption, ADR-0110 D2).
func (r *KindRegistry) ValidateEvent(ev Event) error {
	if r == nil {
		return nil
	}
	et, ok := r.eventTypes[ev.Type]
	if !ok {
		return nil
	}
	have := map[string]bool{}
	for _, role := range ev.Roles {
		if len(et.Roles) > 0 && !containsString(et.Roles, role.Role) {
			return fmt.Errorf("event type %q does not declare role %q (declared: %s): %w",
				ev.Type, role.Role, strings.Join(et.Roles, ", "), ErrKindRefused)
		}
		have[role.Role] = true
	}
	for _, rr := range et.RequiredRoles {
		if !have[rr] {
			return fmt.Errorf("event type %q requires role %q, which the event does not carry: %w", ev.Type, rr, ErrKindRefused)
		}
	}
	return nil
}

func textValue(vs []StatementValue, predicate string) (string, bool) {
	for _, v := range vs {
		if v.Predicate == predicate && v.Type == "text" {
			return v.Text, true
		}
	}
	return "", false
}

func hasValue(vs []StatementValue, predicate string) bool {
	for _, v := range vs {
		if v.Predicate == predicate {
			return true
		}
	}
	return false
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
		if len(vs.Enum) > 0 && !containsString(vs.Enum, v.Text) {
			return fmt.Errorf("kind %q predicate %q: value %q is not one of the declared values (%s): %w",
				kind, v.Predicate, v.Text, strings.Join(vs.Enum, ", "), ErrKindRefused)
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
