package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ADR-0136 D4/D5 as amended by A1 — the fact layer ON THE KNOWLEDGE SUBSTRATE.
//
// A fact is one atomic statement an LLM extracted from an ingested session/document
// ({subject, predicate, value} plus when it held), stored as ONE append-only knowledge
// item of the OSS-core kind `memory_fact` (KindMemoryFact). It is a KEY into the chunk it
// came from (K = V + fact): the raw chunk stays the evidence, and a fact hit contributes
// its SOURCE CHUNK to retrieval.
//
// Items never change. A fact's valid-time interval and its successor are DERIVED by the
// `valid_time_supersession` resolution authority (ValidTimeSupersessionAuthority) from
// the full item set of its resolution key plus stored inputs — the predicate's
// cardinality profile and the assessments that cite the items — and the substrate's
// resolutions table versions the derived answer in system time. Nothing is deleted by
// supersession; erasure is a different act (ADR-0137) that leaves a content-free
// `erased` sentinel so a closed chain stays closed (A1-Q4).
//
// Resolution key (A1-Q3): entity_id = <scope_key>/<subject_key>/<predicate_key>, actor =
// the WRITING principal (A1-Q2). The scope key is a digest of the item's final
// classification, so one principal's statement never closes another classification's.

// Kind, policy and statement-value predicates of memory_fact (A1.1).
const (
	KindMemoryFact              = "memory_fact"
	PolicyValidTimeSupersession = "valid_time_supersession"

	FactPredSubject         = "subject"           // entity: <scope_key>/<subject_key>
	FactPredPredicate       = "predicate"         // text: the attribute as extracted
	FactPredValue           = "value"             // text
	FactPredQuote           = "quote"             // text: the verbatim span cited
	FactPredSourceChunk     = "source_chunk"      // text: the chunk id the fact is a key into
	FactPredSourceDocument  = "source_document"   // text: the document id
	FactPredTurnIndex       = "turn_index"        // number: 0-based passage position
	FactPredSpeaker         = "speaker"           // text: the conversational speaker (a VALUE, not the actor)
	FactPredValidFromSource = "valid_from_source" // text: stated | session | unknown
	FactPredSupersedesPrior = "supersedes_prior"  // number 0/1: the extractor's change-language cue
	FactPredErased          = "erased"            // number 1: the content-free erasure sentinel
	FactPredErasedCloses    = "erased_closes"     // text: the item id the erased statement had closed
)

// Reason codes of the valid_time_supersession authority.
const (
	ReasonFactCurrent = "current_value"
	ReasonFactClosed  = "closed"
	ReasonFactErased  = "erased"
)

// Hit-metadata keys the fact lane writes on a returned chunk (QueryMemory serialises
// metadata to the caller): the matched facts as reader-facing text — a superseded one
// says "superseded by <value> on <date>" — the best fact cosine, and whether the chunk
// states a chain's current value.
const (
	MetaFactMatches    = "fact_matches"
	MetaFactMatchScore = "fact_match"
	MetaFactCurrent    = "fact_current"
)

// FactCardinality is the predicate profile's answer for a fact's predicate.
type FactCardinality string

const (
	FactSingle  FactCardinality = CardinalitySingle
	FactMulti   FactCardinality = CardinalityMulti
	FactUnknown FactCardinality = CardinalityUnknown
)

// FactPolarity is whether the statement asserts or denies the value.
type FactPolarity string

const (
	FactPositive FactPolarity = "positive"
	FactNegative FactPolarity = "negative"
)

// FactValidFromSource says where a fact's valid-time lower bound came from.
type FactValidFromSource string

const (
	// ValidFromStated: the text stated (or implied, resolved against the session time)
	// when it began to hold — Graphiti's prompt rules (ADR-0136 D4).
	ValidFromStated FactValidFromSource = "stated"
	// ValidFromSession: the statement gave no start but carried a change-language cue
	// (supersedes_prior, A1.2 rule 1), so the session's event time is the lower bound.
	ValidFromSession FactValidFromSource = "session"
	// ValidFromUnknown: no start (a planned event, or an unknown one).
	ValidFromUnknown FactValidFromSource = "unknown"
)

// Fact is the READ VIEW of one memory_fact item: its statement values plus, when it was
// read through a derivation, its derived interval. It is never persisted as such.
type Fact struct {
	ID         string // the knowledge item id
	EntityID   string // <scope_key>/<subject_key>/<predicate_key>
	Actor      string // asserted_by: the writing principal
	EvidenceID string // the ADR-0105 evidence row ("" when capture was off)

	Subject   string
	Attribute string
	Value     string
	// The normalised keys the resolution key is built from. Derived, never authored.
	SubjectKey   string
	AttributeKey string
	ValueNorm    string

	// Valid time [ValidFrom, ValidTo). As written, the item's STATED bounds; after a
	// derivation (ApplyInterval), the derived ones. nil = unbounded on that side.
	// ValidEmpty: overtaken at its own start.
	ValidFrom       *time.Time
	ValidTo         *time.Time
	ValidEmpty      bool
	ValidFromSource FactValidFromSource

	// AssertedAt is the session event time — the speaker's own clock (memo §8).
	AssertedAt time.Time
	// SupersededBy is the item that closed this one in the derived chain.
	SupersededBy string
	// Current: the derivation says this value holds now.
	Current bool

	Cardinality     FactCardinality
	Polarity        FactPolarity
	SupersedesPrior bool
	Speaker         string
	// TurnIndex is the 0-based position of the source chunk in its document (-1 =
	// unknown). Quote is the verbatim span the extractor cited; extraction drops a fact
	// whose quote does not occur in its source chunk.
	TurnIndex int
	Quote     string

	SourceChunkID    string
	SourceDocumentID string

	// Tags is the classification: a HINT (the source chunk's tags) when built by the
	// extractor, the decision point's answer once stored. ScopeKey digests it.
	Tags     []string
	ScopeKey string

	Embedding []float32
}

// Open reports whether the fact's valid interval is open-ended.
func (f Fact) Open() bool { return !f.ValidEmpty && f.ValidTo == nil }

// FactHit is one fact returned by a fact search, with the cosine of its embedding to
// the query and — when the derivation closed it — the value and time of its successor,
// so the reader can be told "superseded by <value> on <date>".
type FactHit struct {
	Fact              Fact
	Score             float64
	SupersededByValue string
	SupersededAt      *time.Time
}

// FactSearchOptions scopes a fact search. Scope is the caller's resolved read
// predicate; the enforcing reader refuses a search with none (fail closed), exactly as
// the chunk Search does, and the adapter applies it IN SQL to the item classification.
//
// SourceDocumentIDs (ADR-0136 A4, the document-scoped lane) restricts the search to facts
// whose source document is one of these ids — the document parent the fact recorded at
// extraction (FactPredSourceDocument), an opaque id never parsed. Empty means no
// restriction (the global lane). It narrows the search; Scope still applies in full.
type FactSearchOptions struct {
	TopK              int
	Scope             *TagPredicate
	SourceDocumentIDs []string
}

// NormalizeFactText is the key normalisation for subjects and values: lower case,
// letters and digits kept, every other run collapsed to one space, trimmed.
// "The  Tesla, Model-3!" → "the tesla model 3".
func NormalizeFactText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
			continue
		}
		space = true
	}
	return b.String()
}

// NormalizeFactAttribute is NormalizeFactText with the words joined by '_', the form
// the attribute registry is written in ("Home City" → "home_city").
func NormalizeFactAttribute(s string) string {
	return strings.ReplaceAll(NormalizeFactText(s), " ", "_")
}

// FactScopeKey is the digest of a classification tag set (order and duplicates do not
// matter). "" for an untagged fact.
func FactScopeKey(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	set := map[string]bool{}
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			set[t] = true
		}
	}
	if len(set) == 0 {
		return ""
	}
	keys := make([]string, 0, len(set))
	for t := range set {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x1f")))
	return hex.EncodeToString(sum[:16])
}

// FactEntityID is the resolution key of a fact (A1-Q3). The normalisers emit only
// letters, digits, spaces and '_', so '/' cannot occur inside a component.
func FactEntityID(scopeKey, subjectKey, predicateKey string) string {
	return scopeKey + "/" + subjectKey + "/" + predicateKey
}

// FactSubjectRef is the entity-typed `subject` value: <scope_key>/<subject_key>. It is
// what "every fact about subject S" (the fuzzy candidates) reads through, and it keeps
// that read inside one classification.
func FactSubjectRef(scopeKey, subjectKey string) string {
	return scopeKey + "/" + subjectKey
}

// FactPredicateOfEntity returns the predicate key of a fact entity id.
func FactPredicateOfEntity(entityID string) string {
	if i := strings.LastIndexByte(entityID, '/'); i >= 0 {
		return entityID[i+1:]
	}
	return ""
}

// FactItemID is the deterministic id of a fact item (A1.1; AGENTS.md §2.6):
// ki_fact:<sha256(evidence_id|source_chunk_id|subject_key|predicate_key|value_norm)>.
// A re-ingest that extracts the same statement from the same chunk of the same evidence
// writes the same id; a different value is a different item.
func FactItemID(evidenceID, sourceChunkID, subjectKey, predicateKey, valueNorm string) string {
	sum := sha256.Sum256([]byte(evidenceID + "|" + sourceChunkID + "|" + subjectKey + "|" + predicateKey + "|" + valueNorm))
	return "ki_fact:" + hex.EncodeToString(sum[:])
}

// FactSourceRef is the item's source_ref: <source_chunk_id>|<value_norm>. Together with
// (namespace, kind, entity_id) it is the substrate's idempotency key
// (knowledge_items_source_ref_uniq), so a replayed write is a no-op.
func FactSourceRef(sourceChunkID, valueNorm string) string {
	return sourceChunkID + "|" + valueNorm
}

// MemoryFactKindSpec is core's declaration of the memory_fact kind (A1.6). app.go appends
// it to the plugin-contributed kinds before NewKindRegistry; a plugin declaring the same
// kind fails boot (kinds.go duplicate refusal).
func MemoryFactKindSpec() KindSpec {
	zero, one := 0.0, 1.0
	return KindSpec{
		Kind:   KindMemoryFact,
		Policy: PolicyValidTimeSupersession,
		Predicates: map[string]ValueSpec{
			FactPredSubject:         {Type: "entity"},
			FactPredPredicate:       {Type: "text"},
			FactPredValue:           {Type: "text"},
			FactPredQuote:           {Type: "text"},
			FactPredSourceChunk:     {Type: "text"},
			FactPredSourceDocument:  {Type: "text"},
			FactPredTurnIndex:       {Type: "number", Min: &zero},
			FactPredSpeaker:         {Type: "text"},
			FactPredValidFromSource: {Type: "text"},
			FactPredSupersedesPrior: {Type: "number", Min: &zero, Max: &one},
			FactPredErased:          {Type: "number", Min: &one, Max: &one},
			FactPredErasedCloses:    {Type: "text"},
		},
	}
}

// ToItem renders the fact as the memory_fact item to write. The id, entity id, source
// ref and subject reference are recomputed from the fact's keys and tags, so a caller
// cannot write an item whose identity disagrees with its content.
func (f Fact) ToItem(namespace string) KnowledgeItem {
	scope := FactScopeKey(f.Tags)
	it := KnowledgeItem{
		ID:             KnowledgeItemID(FactItemID(f.EvidenceID, f.SourceChunkID, f.SubjectKey, f.AttributeKey, f.ValueNorm)),
		NamespaceID:    namespace,
		Kind:           KindMemoryFact,
		EvidenceID:     EvidenceID(f.EvidenceID),
		EntityID:       FactEntityID(scope, f.SubjectKey, f.AttributeKey),
		AssertedBy:     f.Actor,
		AssertedAt:     f.AssertedAt,
		SourceRef:      FactSourceRef(f.SourceChunkID, f.ValueNorm),
		Negation:       f.Polarity == FactNegative,
		Classification: append([]string(nil), f.Tags...),
	}
	if f.ValidFrom != nil {
		it.ValidFrom = f.ValidFrom.UTC()
	}
	if f.ValidTo != nil {
		it.ValidTo = f.ValidTo.UTC()
	}
	src := f.ValidFromSource
	if src == "" {
		src = ValidFromUnknown
	}
	cue := 0.0
	if f.SupersedesPrior {
		cue = 1
	}
	text := func(p, v string) StatementValue { return StatementValue{Predicate: p, Type: "text", Text: v} }
	it.Values = []StatementValue{
		{Predicate: FactPredSubject, Type: "entity", EntityRef: FactSubjectRef(scope, f.SubjectKey)},
		text(FactPredPredicate, f.Attribute),
		text(FactPredValue, f.Value),
		text(FactPredQuote, f.Quote),
		text(FactPredSourceChunk, f.SourceChunkID),
		text(FactPredValidFromSource, string(src)),
		{Predicate: FactPredSupersedesPrior, Type: "number", Number: cue},
	}
	if f.SourceDocumentID != "" {
		it.Values = append(it.Values, text(FactPredSourceDocument, f.SourceDocumentID))
	}
	if f.Speaker != "" {
		it.Values = append(it.Values, text(FactPredSpeaker, f.Speaker))
	}
	if f.TurnIndex >= 0 {
		it.Values = append(it.Values, StatementValue{Predicate: FactPredTurnIndex, Type: "number", Number: float64(f.TurnIndex)})
	}
	return it
}

// RekeyFactItem recomputes a memory_fact item's classification-derived identity — its
// entity id and its subject reference — from its CURRENT classification. The write
// chokepoint calls it after ClassifyWrite replaced the hint, so the resolution key
// follows the authoritative classification, never the hint (authz.EnforcingKnowledgeWriter).
// Items of other kinds are returned unchanged.
func RekeyFactItem(it KnowledgeItem) KnowledgeItem {
	if it.Kind != KindMemoryFact {
		return it
	}
	parts := strings.SplitN(it.EntityID, "/", 3)
	if len(parts) != 3 {
		return it
	}
	scope := FactScopeKey(it.Classification)
	it.EntityID = FactEntityID(scope, parts[1], parts[2])
	vals := make([]StatementValue, len(it.Values))
	copy(vals, it.Values)
	for i := range vals {
		if vals[i].Predicate == FactPredSubject && vals[i].Type == "entity" {
			vals[i].EntityRef = FactSubjectRef(scope, parts[1])
		}
	}
	it.Values = vals
	return it
}

// FactFromItem reads a memory_fact item into its view. The interval fields are the
// item's STATED bounds; ApplyInterval overlays a derivation.
func FactFromItem(it KnowledgeItem) Fact {
	f := Fact{
		ID: string(it.ID), EntityID: it.EntityID, Actor: it.AssertedBy, EvidenceID: string(it.EvidenceID),
		AssertedAt: it.AssertedAt, Tags: append([]string(nil), it.Classification...),
		TurnIndex: -1, Polarity: FactPositive, ValidFromSource: ValidFromUnknown,
	}
	if it.Negation {
		f.Polarity = FactNegative
	}
	if parts := strings.SplitN(it.EntityID, "/", 3); len(parts) == 3 {
		f.ScopeKey, f.SubjectKey, f.AttributeKey = parts[0], parts[1], parts[2]
	}
	f.Subject = f.SubjectKey
	if !it.ValidFrom.IsZero() {
		t := it.ValidFrom.UTC()
		f.ValidFrom = &t
	}
	if !it.ValidTo.IsZero() {
		t := it.ValidTo.UTC()
		f.ValidTo = &t
	}
	for _, v := range it.Values {
		switch v.Predicate {
		case FactPredPredicate:
			f.Attribute = v.Text
		case FactPredValue:
			f.Value = v.Text
		case FactPredQuote:
			f.Quote = v.Text
		case FactPredSourceChunk:
			f.SourceChunkID = v.Text
		case FactPredSourceDocument:
			f.SourceDocumentID = v.Text
		case FactPredSpeaker:
			f.Speaker = v.Text
		case FactPredValidFromSource:
			f.ValidFromSource = FactValidFromSource(v.Text)
		case FactPredSupersedesPrior:
			f.SupersedesPrior = v.Number >= 1
		case FactPredTurnIndex:
			f.TurnIndex = int(v.Number)
		}
	}
	if f.Attribute == "" {
		f.Attribute = f.AttributeKey
	}
	f.ValueNorm = NormalizeFactText(f.Value)
	return f
}

// ApplyInterval overlays a derived interval onto the view.
func (f Fact) ApplyInterval(iv ItemInterval) Fact {
	f.ValidFrom, f.ValidTo, f.ValidEmpty = iv.From, iv.To, iv.Empty
	if iv.FromInferred && f.ValidFromSource == ValidFromUnknown {
		// The derivation ordered the fact by its assertion time; say so.
		f.ValidFromSource = ValidFromSession
	}
	f.SupersededBy = string(iv.SupersededBy)
	f.Current = iv.Current
	return f
}

// IsFactErasedSentinel reports whether the item is the content-free erasure sentinel.
func IsFactErasedSentinel(it KnowledgeItem) bool {
	for _, v := range it.Values {
		if v.Predicate == FactPredErased && v.Number >= 1 {
			return true
		}
	}
	return false
}

// FactErasedSentinel is the content-free successor an erasure leaves in a memory_fact
// chain (ADR-0137 Q5, A1-Q4) for ONE item the erased statement had closed: it lives in
// the CLOSED item's key (same entity, actor and classification — so a cross-key closure
// stays closed too), is positioned at the instant the erased statement had closed it
// (at), and carries no subject text, value, quote or chunk — only the `erased` marker and
// the id of the item it keeps closed. The authority treats it as that item's closing
// successor, so a value the principal had already corrected never becomes current again
// on re-derivation; it closes nothing else, so erasing one value of a multi predicate
// does not close its neighbours. Its id is a digest of the two ids (deterministic:
// re-erasing is idempotent).
func FactErasedSentinel(erased, closed KnowledgeItem, at time.Time) KnowledgeItem {
	sum := sha256.Sum256([]byte(string(erased.ID) + "|" + string(closed.ID)))
	return KnowledgeItem{
		ID:             KnowledgeItemID("ki_fact_erased:" + hex.EncodeToString(sum[:])),
		NamespaceID:    closed.NamespaceID,
		Kind:           KindMemoryFact,
		EntityID:       closed.EntityID,
		AssertedBy:     closed.AssertedBy,
		AssertedAt:     erased.AssertedAt,
		SourceRef:      "erased|" + hex.EncodeToString(sum[:]),
		Classification: append([]string(nil), closed.Classification...),
		ValidFrom:      at.UTC(),
		Values: []StatementValue{
			{Predicate: FactPredErased, Type: "number", Number: 1},
			{Predicate: FactPredErasedCloses, Type: "text", Text: string(closed.ID)},
		},
	}
}

// ValidTimeSupersessionAuthority is the resolution authority of memory_fact (A1.2). It
// derives a key's chain from the full item set plus stored inputs, and is PURE and
// ORDER-INDEPENDENT: items and inputs are sorted by stable keys before anything is
// decided, so a replay in any order gives the same chain (memo §13).
//
// The rules (ADR-0136 D5, re-expressed over append-only items):
//
//   - Dates decide direction. Each item's start is its stated valid_from, else its
//     assertion (session) time. Items are ordered by (start, asserted_at, source_ref, id).
//   - A later item j CLOSES an earlier item i at j's start when
//     j is the erased sentinel naming i (the chain stays closed, A1-Q4); or
//     j is a negation of i's value; or, for a different value,
//     the predicate is single (declared or learned), or
//     j carries a supersedes_prior cue, or
//     a stored assessment judged (i, j) contradictory.
//     Under multi or unknown cardinality different values otherwise accumulate.
//   - An equal value later is a continuation, never a replacement (the as-built
//     `corroborates` row is gone: a restatement is just another item).
//   - i's interval is [start_i, first closer's start), unless i's own stated end comes
//     first (non-overlapping: nothing closed it). Same instant: the later-ordered value
//     wins and the earlier keeps an empty interval.
//   - Cross-key: an item of ANOTHER key (ResolutionInputs.Related) judged contradictory
//     with i closes i the same way when it starts later; when it starts earlier, it is
//     the one closed — in its own key's derivation.
//   - The winner is the latest-ordered current item (open, not a negation, not a
//     sentinel). None current: reason erased (a sentinel is last), negated (a negation
//     is last) or closed.
type ValidTimeSupersessionAuthority struct{}

var (
	_ InputResolutionAuthority = ValidTimeSupersessionAuthority{}
	_ SentinelOnEraseAuthority = ValidTimeSupersessionAuthority{}
)

// ErasedSentinel implements SentinelOnEraseAuthority. Erasing a sentinel leaves nothing.
func (ValidTimeSupersessionAuthority) ErasedSentinel(erased, closed KnowledgeItem, at time.Time) (KnowledgeItem, bool) {
	if IsFactErasedSentinel(erased) || IsFactErasedSentinel(closed) {
		return KnowledgeItem{}, false
	}
	return FactErasedSentinel(erased, closed, at), true
}

func (ValidTimeSupersessionAuthority) Policy() string { return PolicyValidTimeSupersession }

// Resolve derives with no stored inputs (unknown cardinality, no assessments): only
// cues, negations and sentinels close anything.
func (a ValidTimeSupersessionAuthority) Resolve(items []KnowledgeItem) (*KnowledgeItem, string) {
	out := a.ResolveWith(items, ResolutionInputs{Cardinality: CardinalityUnknown})
	return out.Winner, out.Reason
}

// ProfilePredicate is the predicate key of the items' entity id.
func (ValidTimeSupersessionAuthority) ProfilePredicate(items []KnowledgeItem) string {
	for _, it := range items {
		if p := FactPredicateOfEntity(it.EntityID); p != "" {
			return p
		}
	}
	return ""
}

type chainMember struct {
	it       KnowledgeItem
	value    string // value_norm
	start    time.Time
	inferred bool
	cue      bool
	erased   bool
	target   KnowledgeItemID // an erased sentinel's one closed item
	related  bool
}

func newChainMember(it KnowledgeItem, related bool) chainMember {
	m := chainMember{it: it, related: related, start: it.ValidFrom}
	if m.start.IsZero() {
		m.start, m.inferred = it.AssertedAt, true
	}
	for _, v := range it.Values {
		switch v.Predicate {
		case FactPredValue:
			m.value = NormalizeFactText(v.Text)
		case FactPredSupersedesPrior:
			m.cue = v.Number >= 1
		case FactPredErased:
			m.erased = v.Number >= 1
		case FactPredErasedCloses:
			m.target = KnowledgeItemID(v.Text)
		}
	}
	return m
}

func chainLess(a, b chainMember) bool {
	if !a.start.Equal(b.start) {
		return a.start.Before(b.start)
	}
	if !a.it.AssertedAt.Equal(b.it.AssertedAt) {
		return a.it.AssertedAt.Before(b.it.AssertedAt)
	}
	if a.it.SourceRef != b.it.SourceRef {
		return a.it.SourceRef < b.it.SourceRef
	}
	return a.it.ID < b.it.ID
}

func pairKey(a, b KnowledgeItemID) [2]KnowledgeItemID {
	if b < a {
		a, b = b, a
	}
	return [2]KnowledgeItemID{a, b}
}

var authorityRank = map[string]int{
	AssessmentAuthorityCue:              1,
	AssessmentAuthorityLLMContradiction: 2,
	AssessmentAuthorityOperator:         3,
}

// verdictIndex maps each judged pair to its effective verdict: the strongest authority
// wins; within one authority the latest-created assessment (then the greatest id) wins.
// All of it is stored data, so the index is a pure function of the rows.
func verdictIndex(asms []Assessment) map[[2]KnowledgeItemID]string {
	sorted := append([]Assessment(nil), asms...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if authorityRank[a.Authority] != authorityRank[b.Authority] {
			return authorityRank[a.Authority] < authorityRank[b.Authority]
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	idx := map[[2]KnowledgeItemID]string{}
	for _, a := range sorted { // later entries overwrite earlier: strongest/latest wins
		for _, v := range a.Verdicts {
			if v.A == "" || v.B == "" || v.A == v.B {
				continue
			}
			idx[pairKey(v.A, v.B)] = v.Verdict
		}
	}
	return idx
}

// ResolveWith implements InputResolutionAuthority.
func (ValidTimeSupersessionAuthority) ResolveWith(items []KnowledgeItem, in ResolutionInputs) ResolutionOutcome {
	if len(items) == 0 {
		return ResolutionOutcome{}
	}
	own := make([]chainMember, 0, len(items))
	ownIDs := map[KnowledgeItemID]bool{}
	for _, it := range items {
		if ownIDs[it.ID] {
			continue // a duplicate in the input set is the same item
		}
		ownIDs[it.ID] = true
		own = append(own, newChainMember(it, false))
	}
	sort.Slice(own, func(i, j int) bool { return chainLess(own[i], own[j]) })
	verdicts := verdictIndex(in.Assessments)
	var related []chainMember
	seenRel := map[KnowledgeItemID]bool{}
	for _, r := range in.Related {
		if ownIDs[r.ID] || seenRel[r.ID] {
			continue
		}
		seenRel[r.ID] = true
		related = append(related, newChainMember(r, true))
	}
	sort.Slice(related, func(i, j int) bool { return chainLess(related[i], related[j]) })
	single := in.Cardinality == CardinalitySingle

	// closes reports whether j (ordered after i) ends i.
	closes := func(i, j chainMember) bool {
		if j.erased {
			// A sentinel keeps exactly the item the erased statement had closed closed;
			// an untargeted one (none is written today) closes everything before it.
			return !j.related && (j.target == "" || j.target == i.it.ID)
		}
		if j.related {
			return verdicts[pairKey(i.it.ID, j.it.ID)] == VerdictContradictory && j.value != i.value
		}
		if j.it.Negation {
			return j.value == "" || j.value == i.value
		}
		if j.value == i.value {
			return false // continuation
		}
		if single || j.cue {
			return true
		}
		return verdicts[pairKey(i.it.ID, j.it.ID)] == VerdictContradictory
	}

	out := ResolutionOutcome{Chain: make([]ItemInterval, 0, len(own))}
	var winner *chainMember
	for idx, i := range own {
		iv := ItemInterval{ItemID: i.it.ID, FromInferred: i.inferred}
		from := i.start
		iv.From = &from
		var statedTo *time.Time
		if !i.it.ValidTo.IsZero() && i.it.ValidTo.After(i.start) {
			t := i.it.ValidTo
			statedTo = &t
		}
		iv.To = statedTo
		if !i.erased && !i.it.Negation {
			// The first closer in chain order: own items after i, and related items
			// ordered after i.
			var closer *chainMember
			for k := idx + 1; k < len(own); k++ {
				if closes(i, own[k]) {
					closer = &own[k]
					break
				}
			}
			for k := range related {
				r := &related[k]
				if !chainLess(i, *r) || !closes(i, *r) {
					continue
				}
				if closer == nil || chainLess(*r, *closer) {
					closer = r
				}
				break // related is sorted; the first later closer is the earliest
			}
			if closer != nil && (statedTo == nil || closer.start.Before(*statedTo)) {
				end := closer.start
				iv.To = &end
				iv.Empty = !end.After(i.start)
				iv.SupersededBy = closer.it.ID
			}
			iv.Current = iv.To == nil
		}
		if iv.Current {
			m := own[idx]
			winner = &m
		}
		out.Chain = append(out.Chain, iv)
	}
	switch {
	case winner != nil:
		w := winner.it
		out.Winner, out.Reason = &w, ReasonFactCurrent
	case own[len(own)-1].erased:
		out.Reason = ReasonFactErased
	case own[len(own)-1].it.Negation:
		out.Reason = ReasonNegated
	default:
		out.Reason = ReasonFactClosed
	}
	return out
}

// FactItemReader is the read port of the fact layer over the substrate (A1.5). The
// postgres adapter (PgFactItemReader) applies the read predicate IN SQL to the item
// classification; the kernel reaches it only through authz.EnforcingFactItemReader,
// which refuses a read with no predicate (fail closed). Every returned Fact carries its
// DERIVED interval and its predicate's cardinality.
type FactItemReader interface {
	// SearchFacts returns the TopK memory_fact items nearest to vec under opts.Scope.
	SearchFacts(ctx context.Context, vec []float32, opts FactSearchOptions) ([]FactHit, error)
	// FactChain returns every item of f's resolution key (entity id + actor) under
	// scope, in chain order, with derived intervals.
	FactChain(ctx context.Context, f Fact, scope *TagPredicate) ([]Fact, error)
	// SimilarFacts returns up to k facts about the same subject in the same
	// classification but under a DIFFERENT predicate, nearest to vec — the fuzzy
	// candidates of A1.2 rule 3.
	SimilarFacts(ctx context.Context, f Fact, vec []float32, k int, scope *TagPredicate) ([]Fact, error)
}

// FactStagedReader is the read side the TWO-PHASE backfill needs beyond FactItemReader
// (memory.FactBackfill with execution.facts.backfill_group_concurrency > 1;
// T-20261103-fact-backfill-processes-each-principal-s). That backfill stores every
// window of a classification group before it assesses any, then assesses each window
// as the serial ordered run would have: against the store WITHOUT the items the serial
// run had not written yet. Both reads apply the predicate exactly as FactItemReader does.
type FactStagedReader interface {
	// FactsByID returns the memory_fact items with these ids as STORED (FactFromItem, no
	// derived interval), each with its embedding from the item projection when one is
	// stored under the reader's model. Missing ids are absent from the result.
	FactsByID(ctx context.Context, ids []KnowledgeItemID, scope *TagPredicate) ([]Fact, error)
	// SimilarFactsHiding is SimilarFacts over the store with the hidden items treated
	// as ABSENT — neither ranked nor part of any derivation — and with a deterministic
	// order (distance, then id). With no hidden items it is SimilarFacts.
	SimilarFactsHiding(ctx context.Context, f Fact, vec []float32, k int, scope *TagPredicate, hidden []KnowledgeItemID) ([]Fact, error)
}
