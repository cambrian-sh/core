package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// Live ontology declarations (ADR-0149 D2, D5).
//
// The kind and relation registries were built once at boot and could not change
// without a restart. They still are the validators — immutable values — but a
// namespace's CURRENT pair is now an OntologySnapshot that a live change replaces
// whole (internal/ontology holds them behind one atomic swap). This file is the
// pure half: the declarations document, the snapshot built from it, and the
// classification of a proposed change. Nothing here touches a database.

// OntologyBaseNamespace is the namespace column of the BASE declarations: the
// ones code and deployment config declare at boot (plugin kinds, the
// CAMBRIAN_SUBSTRATE_KINDS kinds, built-ins, the config's relations). Every
// namespace starts from them; an operator's change applies to one namespace.
const OntologyBaseNamespace = "*"

// OntologyDoc is a set of declarations: what one version of the table declares,
// or — overlaid on the base — what a namespace's snapshot holds. Specs are
// identified by name (KindSpec.Kind, RelationSpec.Name).
type OntologyDoc struct {
	Kinds     []KindSpec     `json:"kinds,omitempty"`
	Relations []RelationSpec `json:"relations,omitempty"`
}

// Overlay returns base with each spec of change replacing the base spec of the
// same name, or appended when the name is new. base is not modified, and is not
// deduplicated: a base that declares one name twice still reaches the registry
// constructor, which refuses it.
func (base OntologyDoc) Overlay(change OntologyDoc) OntologyDoc {
	out := OntologyDoc{
		Kinds:     slices.Clone(base.Kinds),
		Relations: slices.Clone(base.Relations),
	}
	for _, k := range change.Kinds {
		if i := slices.IndexFunc(out.Kinds, func(s KindSpec) bool { return s.Kind == k.Kind }); i >= 0 {
			out.Kinds[i] = k
		} else {
			out.Kinds = append(out.Kinds, k)
		}
	}
	for _, r := range change.Relations {
		if i := slices.IndexFunc(out.Relations, func(s RelationSpec) bool { return s.Name == r.Name }); i >= 0 {
			out.Relations[i] = r
		} else {
			out.Relations = append(out.Relations, r)
		}
	}
	return out
}

// Canonical renders the document as JSON with specs sorted by name, so two
// documents declaring the same things compare equal byte for byte.
func (d OntologyDoc) Canonical() ([]byte, error) {
	c := OntologyDoc{Kinds: slices.Clone(d.Kinds), Relations: slices.Clone(d.Relations)}
	sort.SliceStable(c.Kinds, func(i, j int) bool { return c.Kinds[i].Kind < c.Kinds[j].Kind })
	sort.SliceStable(c.Relations, func(i, j int) bool { return c.Relations[i].Name < c.Relations[j].Name })
	return json.Marshal(c)
}

// OntologySnapshot is one namespace's declarations at one version: the document
// and the two immutable registries built from it. Never mutated after
// construction; a change builds a new snapshot.
type OntologySnapshot struct {
	Namespace string
	// Version is the declarations version: the highest table version among the
	// base rows and this namespace's rows that the snapshot includes.
	Version   int64
	Doc       OntologyDoc
	Kinds     *KindRegistry
	Relations *RelationRegistry
}

// NewOntologySnapshot validates doc by building both registries — the same
// constructors, and so the same refusals, as a boot — and stamps them with
// version so every write can record which declarations it was checked against.
func NewOntologySnapshot(namespace string, version int64, doc OntologyDoc, authorities []ResolutionAuthority) (*OntologySnapshot, error) {
	kinds, err := NewKindRegistry(doc.Kinds, authorities)
	if err != nil {
		return nil, err
	}
	rels, err := NewRelationRegistry(doc.Relations)
	if err != nil {
		return nil, err
	}
	kinds.version, rels.version = version, version
	return &OntologySnapshot{Namespace: namespace, Version: version, Doc: doc, Kinds: kinds, Relations: rels}, nil
}

// OntologyChangeClass is how a declarations change treats existing data
// (ADR-0149 D2, after Confluent BACKWARD and Postgres NOT VALID).
type OntologyChangeClass string

const (
	// ChangeAdditive admits everything the old declarations admitted: applied live.
	ChangeAdditive OntologyChangeClass = "additive"
	// ChangeTightening refuses some new writes the old declarations admitted:
	// applied to new writes only; rows already written are reported, not rejected.
	ChangeTightening OntologyChangeClass = "tightening"
	// ChangeBreaking changes what existing data means (a type change, a removed
	// predicate, a different resolution): refused unless a migration accompanies it.
	ChangeBreaking OntologyChangeClass = "breaking"
)

func (c OntologyChangeClass) rank() int {
	switch c {
	case ChangeTightening:
		return 1
	case ChangeBreaking:
		return 2
	}
	return 0
}

// OntologyChange is one classified difference between two documents.
type OntologyChange struct {
	Class OntologyChangeClass
	// Subject names what changed: "kind order", "kind order.status", "relation placed_by".
	Subject string
	Detail  string
}

// MaxChangeClass is the most severe class in changes; additive for none.
func MaxChangeClass(changes []OntologyChange) OntologyChangeClass {
	out := ChangeAdditive
	for _, c := range changes {
		if c.Class.rank() > out.rank() {
			out = c.Class
		}
	}
	return out
}

// ClassifyOntologyChange lists every difference between cur and next, each
// classified. Specs present in cur and absent from next are breaking removals.
// Pure and deterministic (sorted by subject, then detail).
func ClassifyOntologyChange(cur, next OntologyDoc) []OntologyChange {
	var out []OntologyChange
	add := func(c OntologyChangeClass, subject, format string, args ...any) {
		out = append(out, OntologyChange{Class: c, Subject: subject, Detail: fmt.Sprintf(format, args...)})
	}

	oldKinds := map[string]KindSpec{}
	for _, k := range cur.Kinds {
		oldKinds[k.Kind] = k
	}
	newKinds := map[string]bool{}
	for _, k := range next.Kinds {
		newKinds[k.Kind] = true
		old, ok := oldKinds[k.Kind]
		if !ok {
			add(ChangeAdditive, "kind "+k.Kind, "new kind")
			continue
		}
		classifyKind(old, k, add)
	}
	for name := range oldKinds {
		if !newKinds[name] {
			add(ChangeBreaking, "kind "+name, "kind removed")
		}
	}

	oldRels := map[string]RelationSpec{}
	for _, r := range cur.Relations {
		oldRels[r.Name] = r
	}
	newRels := map[string]bool{}
	for _, r := range next.Relations {
		newRels[r.Name] = true
		old, ok := oldRels[r.Name]
		if !ok {
			add(ChangeAdditive, "relation "+r.Name, "new relation")
			continue
		}
		classifyRelation(old, r, add)
	}
	for name := range oldRels {
		if !newRels[name] {
			add(ChangeBreaking, "relation "+name, "relation removed")
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

type addChange func(c OntologyChangeClass, subject, format string, args ...any)

func classifyKind(old, nw KindSpec, add addChange) {
	subject := "kind " + nw.Kind
	// Everything but the predicates decides how existing items RESOLVE (policy,
	// scope, designated set, lifecycle, event types, commit class). Changing it
	// re-interprets data already written, so it is breaking until a later ADR
	// classifies a field of it more finely.
	if !reflect.DeepEqual(kindShell(old), kindShell(nw)) {
		add(ChangeBreaking, subject, "resolution, lifecycle, event-type or commit-class fields changed")
	}
	for p := range old.Predicates {
		if _, ok := nw.Predicates[p]; !ok {
			add(ChangeBreaking, subject+"."+p, "predicate removed")
		}
	}
	for p, nv := range nw.Predicates {
		ps := subject + "." + p
		ov, ok := old.Predicates[p]
		if !ok {
			if nv.Required {
				add(ChangeTightening, ps, "new required predicate")
			} else {
				add(ChangeAdditive, ps, "new optional predicate")
			}
			continue
		}
		if ov.Type != nv.Type {
			add(ChangeBreaking, ps, "value type %s -> %s", ov.Type, nv.Type)
			continue
		}
		switch {
		case !ov.Required && nv.Required:
			add(ChangeTightening, ps, "now required")
		case ov.Required && !nv.Required:
			add(ChangeAdditive, ps, "no longer required")
		}
		classifyEnum(ov.Enum, nv.Enum, ps, add)
		classifyBound("minimum", ov.Min, nv.Min, func(o, n float64) bool { return n > o }, ps, add)
		classifyBound("maximum", ov.Max, nv.Max, func(o, n float64) bool { return n < o }, ps, add)
	}
}

// kindShell is a spec without its predicates and with the default policy
// spelled out, so only the resolution-shaping fields are compared.
func kindShell(s KindSpec) KindSpec {
	s.Predicates = nil
	if s.Policy == "" {
		s.Policy = ResolutionPolicyLatestAssertion
	}
	return s
}

func classifyEnum(old, nw []string, subject string, add addChange) {
	switch {
	case len(old) == 0 && len(nw) == 0:
		return
	case len(old) == 0:
		add(ChangeTightening, subject, "enum declared")
		return
	case len(nw) == 0:
		add(ChangeAdditive, subject, "enum removed")
		return
	}
	var dropped, added []string
	for _, v := range old {
		if !slices.Contains(nw, v) {
			dropped = append(dropped, v)
		}
	}
	for _, v := range nw {
		if !slices.Contains(old, v) {
			added = append(added, v)
		}
	}
	if len(dropped) > 0 {
		add(ChangeTightening, subject, "enum narrowed (removed %v)", dropped)
	}
	if len(added) > 0 {
		add(ChangeAdditive, subject, "enum widened (added %v)", added)
	}
}

// classifyBound: stricter(o, n) reports whether bound n admits less than o.
func classifyBound(name string, old, nw *float64, stricter func(o, n float64) bool, subject string, add addChange) {
	switch {
	case old == nil && nw == nil:
	case old == nil:
		add(ChangeTightening, subject, "%s %g declared", name, *nw)
	case nw == nil:
		add(ChangeAdditive, subject, "%s removed", name)
	case *old == *nw:
	case stricter(*old, *nw):
		add(ChangeTightening, subject, "%s %g -> %g", name, *old, *nw)
	default:
		add(ChangeAdditive, subject, "%s %g -> %g", name, *old, *nw)
	}
}

func classifyRelation(old, nw RelationSpec, add addChange) {
	subject := "relation " + nw.Name
	if old.Family != nw.Family || old.Symmetric != nw.Symmetric || old.Closure != nw.Closure {
		add(ChangeBreaking, subject, "family, symmetry or closure changed")
	}
	endpoint := func(end, o, n string) {
		switch {
		case o == n:
		case n == "":
			add(ChangeAdditive, subject, "%s_kind %q no longer required", end, o)
		default:
			add(ChangeTightening, subject, "%s_kind %q -> %q", end, o, n)
		}
	}
	endpoint("from", old.FromKind, nw.FromKind)
	endpoint("to", old.ToKind, nw.ToKind)
	switch {
	case !old.SingleValued() && nw.SingleValued():
		add(ChangeTightening, subject, "cardinality many -> one")
	case old.SingleValued() && !nw.SingleValued():
		add(ChangeAdditive, subject, "cardinality one -> many")
	}
	switch {
	case !old.Required && nw.Required:
		add(ChangeTightening, subject, "now required")
	case old.Required && !nw.Required:
		add(ChangeAdditive, subject, "no longer required")
	}
	switch {
	case old.Inverse == nw.Inverse:
	case old.Inverse == "":
		add(ChangeAdditive, subject, "inverse %q declared", nw.Inverse)
	default:
		add(ChangeBreaking, subject, "inverse %q -> %q", old.Inverse, nw.Inverse)
	}
}

// ErrOntologyChangeRefused marks a declarations change that was not applied: a
// breaking change with no migration, a change whose result does not validate,
// or a change raced by another writer.
var ErrOntologyChangeRefused = errors.New("ontology change refused")

// OntologyApplyResult reports a proposed change.
type OntologyApplyResult struct {
	Namespace string
	// Version is the namespace's version after the call (unchanged when nothing
	// was applied).
	Version int64
	// Applied is true when a new version was written and swapped in.
	Applied bool
	Class   OntologyChangeClass
	Changes []OntologyChange
}

// Rules an OntologyViolation can name.
const (
	ViolationRequired     = "required"      // a current item lacks a required predicate
	ViolationEnum         = "enum"          // a current value is outside its enum
	ViolationRange        = "range"         // a current number is outside min/max
	ViolationFromKind     = "from_kind"     // a confirmed link's from endpoint is the wrong kind
	ViolationToKind       = "to_kind"       // a confirmed link's to endpoint is the wrong kind
	ViolationCardinality  = "cardinality"   // overlapping confirmed links of a single-valued verb
	ViolationRequiredLink = "required_link" // an entity lacks a required confirmed link
)

// OntologyViolation is one existing row that the CURRENT declarations would
// refuse if it were written now (ADR-0149 D2: reported, never retro-rejected).
type OntologyViolation struct {
	Rule string
	// Subject is the declaration it violates ("kind order.status", "relation placed_by").
	Subject string
	// Ref is the offending row: an item id, a link id, or an entity id.
	Ref    string
	Detail string
}

// Ontology is the live declarations surface a plugin reaches through
// KernelServices (ADR-0149 D2): read a namespace's snapshot, propose or apply a
// change, and read the violations report as a principal (the kernel resolves
// the principal's read scope; a caller never supplies one).
type Ontology interface {
	Current(namespace string) *OntologySnapshot
	Apply(ctx context.Context, namespace string, change OntologyDoc, author string, dryRun bool) (OntologyApplyResult, error)
	// Violations lists up to limit rows the current declarations would refuse;
	// truncated reports that more exist.
	Violations(ctx context.Context, principal PrincipalRef, namespace string, limit int) (v []OntologyViolation, truncated bool, err error)
}
