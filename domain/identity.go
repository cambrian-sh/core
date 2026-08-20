package domain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The identity plane (five-planes step 1+2; FIVE-PLANES-BUILD.md, migration 0016).
//
// Everything in the substrate until now keyed on `entity_id` as bare TEXT. Two sources
// naming the same customer matched only if they happened to spell it the same way, and
// nothing anywhere recorded that a match had been CLAIMED, by whom, or on what basis.
// This file is the vocabulary and the ports for the plane that fixes that: entities are
// minted handles, and links are assertions ABOUT handles.
//
// The split matters. An Entity carries no belief — minting one costs an insert and
// asserts nothing beyond "a thing by this name exists". Every epistemic burden lives on
// the Link: who said it, by what means, on what evidence, when they said it, and when
// the world it describes held. That is why a producer may mint freely and still leave
// every equivalence it proposes reviewable.

// ErrLinkRefused marks a PERMANENT refusal by the identity plane — an undeclared verb, a
// mechanism reaching above its trust ceiling, an assertion with no basis. Like
// ErrKindRefused it separates "this write can never succeed" from "retry me", and for
// the same reason: producers run inside the projection path off the evidence outbox, and
// an at-least-once consumer that cannot tell the two apart turns one bad row into an
// item that retries forever.
//
// It WRAPS ErrKindRefused deliberately. Every consumer that already learned to treat
// ErrKindRefused as terminal (postgres/knowledge_store.go's async lanes) gets link
// refusals right without being taught a second sentinel, while a caller that wants to
// name the identity plane specifically still can.
var ErrLinkRefused = fmt.Errorf("refused by the identity plane: %w", ErrKindRefused)

// Link families — the three questions a link can answer.
const (
	// LinkFamilyIdentity: these two refs are the same thing. Canonically ordered
	// (from_ref < to_ref) by the store, so symmetry is a read-path property rather
	// than two rows no dedup key could reconcile.
	LinkFamilyIdentity = "identity"
	// LinkFamilyRelation: this thing stands in a stated relation to that thing.
	LinkFamilyRelation = "relation"
	// LinkFamilyLineage: this came from / was preceded by that.
	LinkFamilyLineage = "lineage"
)

// Link states — the REVIEW lane, not a confidence bucket. A machine producer proposes
// a candidate; a human promotes it. A rejected candidate is retracted, never deleted,
// because a producer that cannot see its proposal was rejected re-proposes it forever.
const (
	LinkStateCandidate = "candidate"
	LinkStateConfirmed = "confirmed"
	LinkStateRetracted = "retracted"
)

// Link mechanisms — HOW the assertion was made. These names, and the two rules below,
// are the ONE place in the kernel where a mechanism value appears in logic. Everything
// else treats mechanism as data.
const (
	// The source itself said so: a crosswalk column, a declared mapping rule.
	LinkMechanismDeclared = "declared"
	// A record of the source stated the relation as a field.
	LinkMechanismRecord = "record"
	// A text reference resolved to exactly one known entity.
	LinkMechanismReference = "reference"
	// Two occurrences share a participant object.
	LinkMechanismSharedObject = "shared_object"
	// The kernel itself performed or observed the act (effect intents, receipts).
	LinkMechanismWitnessed = "witnessed"
	// A deterministic rule derived it from stored rows.
	LinkMechanismDerived = "derived"
	// A model or similarity score proposed it.
	LinkMechanismScored = "scored"
	// A person asserted it.
	LinkMechanismHuman = "human"
	// Co-occurrence in time or context, with no stated basis.
	LinkMechanismCorrelation = "correlation"
)

// linkMechanismCeiling is the trust ceiling: the highest state each mechanism may write.
//
// {declared, record, reference, shared_object, witnessed, human} state a basis somebody
// or something actually holds, so they may write `confirmed` directly. {derived, scored,
// correlation} are inferences, and an inference that can promote itself into the answer
// plane is the whole failure mode this table exists to prevent — a similarity score does
// not become true by being confident. They cap at `candidate` and wait for a human.
//
// Membership of this map is also the mechanism VOCABULARY: an unknown mechanism is
// refused rather than silently admitted at some default ceiling.
var linkMechanismCeiling = map[string]string{
	LinkMechanismDeclared:     LinkStateConfirmed,
	LinkMechanismRecord:       LinkStateConfirmed,
	LinkMechanismReference:    LinkStateConfirmed,
	LinkMechanismSharedObject: LinkStateConfirmed,
	LinkMechanismWitnessed:    LinkStateConfirmed,
	LinkMechanismHuman:        LinkStateConfirmed,
	LinkMechanismDerived:      LinkStateCandidate,
	LinkMechanismScored:       LinkStateCandidate,
	LinkMechanismCorrelation:  LinkStateCandidate,
}

// Typed-ref prefixes. A link joins any two planes without needing a column per plane,
// so both endpoints are prefixed refs rather than bare ids.
const (
	RefPrefixEntity   = "entity:"
	RefPrefixEvent    = "event:"
	RefPrefixDecision = "decision:"
	RefPrefixEvidence = "evidence:"
)

// EntityRef renders a scoped entity id as the typed ref a link endpoint carries.
func EntityRef(id string) string { return RefPrefixEntity + id }

// Entity is a minted handle: "there is a thing called customer/C-1042". It asserts no
// properties and carries no belief — those live on links and observations. Entities are
// deliberately cheap, so a producer that meets an unseen id mints it rather than
// dropping the row.
type Entity struct {
	// ID is the SCOPED id: prefix stem + local part, "customer/C-1042". Globally
	// unique, because a bare "C-1042" that is a customer in one source and a
	// container in another is exactly the collision the prefix prevents.
	ID          string
	NamespaceID string
	// Kind is the prefix stem ("customer"), stored so it is indexable. The store
	// derives it from the ID when empty and validates only WELL-FORMEDNESS
	// (amendment S3) — the vocabulary is a deployment concern, checked at
	// mapping-confirm time, not a kernel registry.
	Kind string
	// FirstSeenEvidence is the delivery that first caused this handle to exist.
	// Empty for an entity minted by an operator or a background pass.
	FirstSeenEvidence EvidenceID
	CreatedAt         time.Time
}

// Link is one ASSERTION about two typed refs. Not a fact: every field except the
// endpoints exists to record the provenance of a claim, so that a claim can later be
// reviewed, retracted, or revoked in a batch when its producer turns out to be wrong.
type Link struct {
	ID          string
	NamespaceID string
	// Family is identity | relation | lineage.
	Family string
	// FromRef/ToRef are typed refs ("entity:customer/C-1042"). For the identity
	// family the store canonically orders them FromRef < ToRef.
	FromRef string
	ToRef   string
	// Relation is the verb. DATA — declared in the RelationRegistry; the kernel
	// never branches on a specific value.
	Relation string
	// State is candidate | confirmed | retracted. Defaults to candidate.
	State string
	// Mechanism is HOW the assertion was made; it carries the trust ceiling.
	Mechanism string
	// Producer is name@version of the producing pass — the batch revocation key.
	Producer string
	// Confidence defaults to 1.0. It orders the review queue; it does NOT lift a
	// mechanism's ceiling.
	Confidence float64
	// EvidenceID is the basis. Required for every non-human mechanism (the
	// admissibility rule): a machine that cannot say why it believes something has
	// not made an assertion.
	EvidenceID EvidenceID
	AssertedBy string
	AssertedAt time.Time
	RecordedAt time.Time
	// ValidFrom/ValidTo bound when the described relation HELD, as distinct from when
	// it was asserted — the bi-temporal pair, so "who owned this in March" survives a
	// later reassignment. nil = unbounded on that side.
	ValidFrom   *time.Time
	ValidTo     *time.Time
	RetractedAt *time.Time
	// SourceRef is the producing source's native reference, UNQUALIFIED — no
	// "@r<rev>" suffix (amendment S1), so a mapping-revision bump re-derives the same
	// link as a no-op instead of duplicating the whole graph.
	SourceRef string
}

// LinkQuery filters a LinksFor read. Every field's zero value means "no filter", so the
// common walk is LinksFor(ctx, ns, ref, LinkQuery{}).
type LinkQuery struct {
	// Family, State and Relation each filter exactly when non-empty.
	Family   string
	State    string
	Relation string
	// IncludeRetracted admits rows with retracted_at set. Off by default: a
	// traversal that follows retractions has not honoured the retraction.
	IncludeRetracted bool
	// IncludeIncoming admits rows where the ref is the TO endpoint of a
	// non-symmetric verb — the backward walk (`why`). Rows whose verb the registry
	// declares Symmetric are returned in both directions regardless, because for a
	// symmetric verb "incoming" is not a distinct thing; canonical ordering merely
	// decided which column the ref landed in.
	IncludeIncoming bool
	// Limit caps the result; 0 = the store's default cap.
	Limit int
}

// EntityStore mints and reads entity handles (migration 0016). Minting is idempotent
// and deliberately unopinionated — everything that could be wrong about an entity is a
// link, and links are where review happens.
type EntityStore interface {
	// EnsureEntity mints the handle if absent and does nothing if present. Returns
	// created=false on a replay. Kind is derived from the id's prefix stem when
	// empty; a malformed stem is refused with ErrLinkRefused (permanent).
	EnsureEntity(ctx context.Context, e Entity) (created bool, err error)

	// GetEntity returns the handle, or ok=false when it was never minted — "no such
	// entity" is an answer, never an error.
	GetEntity(ctx context.Context, namespace, id string) (Entity, bool, error)

	// ListEntitiesByKind returns minted handles of one kind, newest first.
	ListEntitiesByKind(ctx context.Context, namespace, kind string, limit int) ([]Entity, error)

	// ResolveEntityByLocal returns the minted handles whose id ends in
	// "/"+local — every kind that happens to use this local id (five-planes
	// step 2 / build doc P2, supervisor decision D-W2-2).
	//
	// It exists for ONE caller shape: a producer holding an id-shaped token it
	// read out of prose ("see OPS-0412") and no kind to scope it with. The
	// question it answers is deliberately narrow, and so is the answer —
	// exactly one hit means the token names that entity; zero or many means it
	// does not name one, and the producer records the ambiguity rather than
	// picking. That is why this returns a LIST and not an entity: a resolver
	// that returned "the best match" would be inventing the identity claim the
	// review lane exists to keep reviewable.
	//
	// limit caps the page; 0 = the store's default.
	ResolveEntityByLocal(ctx context.Context, namespace, local string, limit int) ([]Entity, error)
}

// LinkStore persists and reads assertions (migration 0016). It is the enforcement point
// for the three refusals — undeclared verb, trust ceiling, admissibility — because a
// refusal that lived in a producer would only bind the producers that remembered it.
type LinkStore interface {
	// PutLink appends one assertion, idempotent on links_dedup: the same producer
	// replaying the same delivery returns created=false and writes nothing, while a
	// DIFFERENT mechanism or source asserting the same thing writes a second row
	// (corroboration is information; read paths deduplicate).
	//
	// Refuses with ErrLinkRefused when the verb is undeclared, when the mechanism
	// reaches above its trust ceiling, or when a non-human mechanism carries no
	// evidence. Identity-family endpoints are canonically ordered before the write.
	PutLink(ctx context.Context, l Link) (created bool, err error)

	// ConfirmLink promotes a candidate by writing a NEW row — mechanism `human`,
	// state `confirmed`, asserted_by the actor, evidence inherited from the row being
	// confirmed. The original is left EXACTLY as it was: what the machine proposed
	// and what the human decided are two assertions, and overwriting the first would
	// destroy the record of the machine's behaviour that the review lane exists to
	// build. Re-confirming is idempotent (the confirmation's source_ref is derived
	// from the confirmed row's id) and returns the existing confirmation.
	ConfirmLink(ctx context.Context, namespace, linkID, actor string) (Link, error)

	// RetractLink stamps state and retracted_at, and NOTHING else — never a DELETE,
	// never another column. actor and reason are for the caller's audit record (the
	// operator mutation lane already writes command_id + reason); the link row itself
	// never gains a field, because an existing row that can be rewritten is a row
	// whose history cannot be trusted (ADR-0093 D6).
	RetractLink(ctx context.Context, namespace, linkID, actor, reason string) error

	// LinksFor returns the assertions touching a typed ref, subject to opts.
	LinksFor(ctx context.Context, namespace, ref string, opts LinkQuery) ([]Link, error)

	// Candidates is the review inbox: unretracted candidates, highest confidence
	// first.
	Candidates(ctx context.Context, namespace string, limit int) ([]Link, error)

	// RetractByProducer revokes every unretracted row a producer wrote and returns
	// the count. The reason the producer column exists: a pass that turns out to be
	// wrong is undone wholesale rather than row by row.
	RetractByProducer(ctx context.Context, namespace, producer, actor string) (int, error)
}

// RelationSpec declares one verb. Specs are DATA: the kernel learns that `same_as` is
// symmetric and closes over identity, never what "same as" MEANS.
type RelationSpec struct {
	// Name is the verb ("subsidiary_of").
	Name string
	// Family is identity | relation | lineage — the family every link using this verb
	// must carry.
	Family string
	// Symmetric makes the verb hold in both directions; read paths return such links
	// for either endpoint.
	Symmetric bool
	// Closure is "identity" | "rollup" | "": whether an entity-scoped read may expand
	// through this verb, and how. Empty = never expand.
	Closure string
	// MaxPerEntity bounds the fan-out; 0 = unlimited. Exceeding it FLAGS the entity —
	// the cap is never raised silently to fit the data.
	MaxPerEntity int
}

// Built-in seed verbs. Registered at boot exactly as ResolutionPolicyLatestAssertion is:
// the two verbs the kernel's own lanes emit must exist in every deployment, whether or
// not a plugin declares anything.
const (
	// RelationSameAs is the identity verb — the one the whole plane exists for.
	RelationSameAs = "same_as"
	// RelationPrecededAndSharesEntities is the ONLY verb a correlation producer may
	// use, and its name is the honest description of what co-occurrence establishes:
	// order plus overlap, not cause. Closure is empty, so it never widens a read.
	RelationPrecededAndSharesEntities = "preceded_and_shares_entities"
)

// Closure kinds a RelationSpec may declare.
const (
	ClosureIdentity = "identity"
	ClosureRollup   = "rollup"
)

func builtinRelationSpecs() map[string]RelationSpec {
	return map[string]RelationSpec{
		RelationSameAs: {
			Name:      RelationSameAs,
			Family:    LinkFamilyIdentity,
			Symmetric: true,
			Closure:   ClosureIdentity,
		},
		RelationPrecededAndSharesEntities: {
			Name:   RelationPrecededAndSharesEntities,
			Family: LinkFamilyLineage,
		},
	}
}

// RelationRegistry validates link verbs against what the deployment declared. Immutable
// after construction, for the KindRegistry's reason: a registry that can gain verbs
// mid-flight can disagree with itself between two writes of one batch.
type RelationRegistry struct {
	relations map[string]RelationSpec
}

// NewRelationRegistry builds the registry over the built-in seeds. Refuses at STARTUP —
// the chunker-registry rule, never a silent fallback — on a duplicate verb (two owners
// for one verb is a fight the boot must referee), a verb that redeclares a built-in, an
// unknown family or closure, or a negative cap.
func NewRelationRegistry(specs []RelationSpec) (*RelationRegistry, error) {
	rel := builtinRelationSpecs()
	for _, s := range specs {
		if s.Name == "" {
			return nil, fmt.Errorf("relation registry: a spec must name its verb")
		}
		if _, builtin := builtinRelationSpecs()[s.Name]; builtin {
			return nil, fmt.Errorf("relation registry: verb %q is built in and cannot be redeclared", s.Name)
		}
		if _, dup := rel[s.Name]; dup {
			return nil, fmt.Errorf("relation registry: verb %q declared twice", s.Name)
		}
		switch s.Family {
		case LinkFamilyIdentity, LinkFamilyRelation, LinkFamilyLineage:
		default:
			return nil, fmt.Errorf("relation registry: verb %q declares unknown family %q (want %s|%s|%s)",
				s.Name, s.Family, LinkFamilyIdentity, LinkFamilyRelation, LinkFamilyLineage)
		}
		switch s.Closure {
		case "", ClosureIdentity, ClosureRollup:
		default:
			return nil, fmt.Errorf("relation registry: verb %q declares unknown closure %q (want %s|%s or empty)",
				s.Name, s.Closure, ClosureIdentity, ClosureRollup)
		}
		if s.MaxPerEntity < 0 {
			return nil, fmt.Errorf("relation registry: verb %q declares a negative MaxPerEntity", s.Name)
		}
		rel[s.Name] = s
	}
	return &RelationRegistry{relations: rel}, nil
}

// Spec returns the declaration for a verb. A nil registry still knows the built-in
// seeds — the KindRegistry.Authority precedent: the kernel's own lanes must keep working
// in a deployment that declared nothing.
func (r *RelationRegistry) Spec(name string) (RelationSpec, bool) {
	if r == nil {
		s, ok := builtinRelationSpecs()[name]
		return s, ok
	}
	s, ok := r.relations[name]
	return s, ok
}

// SymmetricVerbs lists every declared verb that holds in both directions, sorted. Read
// paths pass this SET to SQL rather than naming a verb, which is what keeps "return both
// directions for same_as" out of the kernel as a branch.
func (r *RelationRegistry) SymmetricVerbs() []string {
	return r.verbsWhere(func(s RelationSpec) bool { return s.Symmetric })
}

// ClosureVerbs lists every declared verb whose closure kind matches, sorted. Wave-2's
// alias expansion asks the registry which verbs it may walk; it never names one.
func (r *RelationRegistry) ClosureVerbs(closure string) []string {
	return r.verbsWhere(func(s RelationSpec) bool { return s.Closure == closure })
}

// Verbs lists every declared verb, sorted — for refusal messages and operator surfaces.
func (r *RelationRegistry) Verbs() []string {
	return r.verbsWhere(func(RelationSpec) bool { return true })
}

func (r *RelationRegistry) verbsWhere(keep func(RelationSpec) bool) []string {
	src := builtinRelationSpecs()
	if r != nil {
		src = r.relations
	}
	out := make([]string, 0, len(src))
	for name, s := range src {
		if keep(s) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateLink applies the three permanent refusals the identity plane owes every
// caller, in the order a reviewer would ask them:
//
//	is this verb declared         → the vocabulary must be a deployment's decision
//	may this mechanism say that   → the trust ceiling
//	on what basis                 → the admissibility rule
//
// It lives in the domain because the mechanism names must appear in exactly one place,
// and it is CALLED from the store because a rule enforced anywhere else only binds the
// callers that remembered it. Pure, so the semantics are testable without a database.
func (r *RelationRegistry) ValidateLink(l Link) error {
	spec, declared := r.Spec(l.Relation)
	if !declared {
		return fmt.Errorf("link relation %q is not declared (declared: %s): %w",
			l.Relation, strings.Join(r.Verbs(), ", "), ErrLinkRefused)
	}
	if l.Family != spec.Family {
		return fmt.Errorf("link relation %q belongs to family %q, asserted under %q: %w",
			l.Relation, spec.Family, l.Family, ErrLinkRefused)
	}
	switch l.State {
	case LinkStateCandidate, LinkStateConfirmed, LinkStateRetracted:
	default:
		return fmt.Errorf("link state %q is not %s|%s|%s: %w",
			l.State, LinkStateCandidate, LinkStateConfirmed, LinkStateRetracted, ErrLinkRefused)
	}
	ceiling, known := linkMechanismCeiling[l.Mechanism]
	if !known {
		return fmt.Errorf("link mechanism %q is not a known mechanism: %w", l.Mechanism, ErrLinkRefused)
	}
	if l.State == LinkStateConfirmed && ceiling != LinkStateConfirmed {
		return fmt.Errorf("mechanism %q may assert at most %q, not %q — an inference does not "+
			"become true by being confident: %w", l.Mechanism, ceiling, l.State, ErrLinkRefused)
	}
	if l.Mechanism != LinkMechanismHuman && l.EvidenceID == "" {
		return fmt.Errorf("mechanism %q asserted with no evidence — a machine that cannot say why "+
			"it believes something has not made an assertion: %w", l.Mechanism, ErrLinkRefused)
	}
	if l.FromRef == "" || l.ToRef == "" || l.AssertedBy == "" {
		return fmt.Errorf("a link needs from_ref, to_ref and asserted_by: %w", ErrLinkRefused)
	}
	return nil
}

// CanonicalizeLink orders the endpoints of an IDENTITY link so from_ref < to_ref, and
// leaves every other family exactly as asserted (direction is the meaning there).
//
// Without this, "A same_as B" and "B same_as A" are two rows that no dedup key could
// ever reconcile and no read path could ever deduplicate — the same equivalence counted
// twice, and a reviewer asked to confirm it twice. Pure and total, so the property is
// provable without a database.
func CanonicalizeLink(l Link) Link {
	if l.Family == LinkFamilyIdentity && l.FromRef > l.ToRef {
		l.FromRef, l.ToRef = l.ToRef, l.FromRef
	}
	return l
}

// EntityKindFromID returns the prefix stem of a scoped entity id: "customer/C-1042" →
// "customer". ok=false when the id carries no stem, which is a refusal, not a default —
// an unscoped id is the collision the scoping exists to prevent.
func EntityKindFromID(id string) (string, bool) {
	i := strings.Index(id, "/")
	if i <= 0 || i == len(id)-1 {
		return "", false
	}
	return id[:i], true
}

// ValidateEntityKind checks WELL-FORMEDNESS only — non-empty, lowercase snake, no "/"
// (amendment S3). Deliberately not a vocabulary check: the kind registry is immutable
// and boot-scoped, and a deployment cannot enumerate every entity kind its sources will
// ever carry any more than ADR-0121 could enumerate every customer. Vocabulary is
// checked where a human is present — at mapping-confirm time in the studio.
func ValidateEntityKind(kind string) error {
	if kind == "" {
		return fmt.Errorf("entity kind is empty: %w", ErrLinkRefused)
	}
	for i, c := range kind {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_' && i > 0:
		default:
			return fmt.Errorf("entity kind %q is not lowercase snake_case: %w", kind, ErrLinkRefused)
		}
	}
	return nil
}
