package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// ADR-0136 Amendment A1 — the substrate pieces the fact layer needs and other kinds can
// reuse: the Assessment stage of the epistemic pipeline (memo §6), an input-taking
// resolution authority, and learned predicate profiles.
//
//	Evidence → KnowledgeItem → Assessment → Resolution → Effect
//
// Until A1 the Assessment stage existed only in the memo. An assessment is a JUDGE's
// recorded verdict over a named candidate set: which authority, which version, which
// prompt, which items, what it said. Its id is a hash of exactly those inputs, so the
// same judge over the same candidates is one row, and a replay READS the verdict instead
// of asking again — the keyed-judge property TOKI (arXiv 2606.06240) shows is necessary
// for replay consistency. A verdict is never edited; it is overruled by a later
// assessment of a stronger authority (operator).

// Assessment verdicts (memo §6 vocabulary plus same_predicate).
const (
	// VerdictContradictory: the two items cannot both hold at once — one replaces the
	// other. The authority decides WHICH by dates, never the judge.
	VerdictContradictory = "contradictory"
	// VerdictConsistent: both can hold at once.
	VerdictConsistent = "consistent"
	// VerdictSamePredicate: two predicate names mean the same thing (an alias proposal).
	VerdictSamePredicate = "same_predicate"
	// VerdictUncertain / VerdictRefused: the judge could not or would not decide.
	VerdictUncertain = "uncertain"
	VerdictRefused   = "refused"
)

// Assessment authorities.
const (
	AssessmentAuthorityLLMContradiction = "llm_contradiction"
	AssessmentAuthorityCue              = "cue"
	AssessmentAuthorityOperator         = "operator"
)

// AssessmentVerdict is one judged pair.
type AssessmentVerdict struct {
	A       KnowledgeItemID `json:"a"`
	B       KnowledgeItemID `json:"b"`
	Verdict string          `json:"verdict"`
}

// Assessment is one stored judge verdict over a candidate set (table
// knowledge_assessments, migration 0022).
type Assessment struct {
	ID               string
	NamespaceID      string
	Kind             string
	Authority        string
	AuthorityVersion string
	PromptHash       string
	InputItemIDs     []KnowledgeItemID
	Verdicts         []AssessmentVerdict
	RawOutput        string
	// Classification equals the inputs' classification: candidates never cross a
	// scope key, so one assessment never mixes two classifications.
	Classification []string
	CreatedAt      time.Time
}

// AssessmentID is the deterministic, keyed id of an assessment (AGENTS.md §2.6):
// sha256(authority|authority_version|prompt_hash|sorted input item ids). Input order
// and duplicates do not matter.
func AssessmentID(authority, authorityVersion, promptHash string, inputs []KnowledgeItemID) string {
	ids := make([]string, 0, len(inputs))
	seen := map[KnowledgeItemID]bool{}
	for _, id := range inputs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, string(id))
		}
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(authority + "|" + authorityVersion + "|" + promptHash + "|" + strings.Join(ids, "\x1f")))
	return "asm:" + hex.EncodeToString(sum[:])
}

// PromptHash is the digest an assessment records of the exact prompt the judge saw.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Cardinality values of a predicate profile.
const (
	CardinalitySingle  = "single"
	CardinalityMulti   = "multi"
	CardinalityUnknown = "unknown"
)

// Predicate profile sources, in increasing precedence: learned < declared < override.
const (
	ProfileSourceLearned  = "learned"
	ProfileSourceDeclared = "declared"
	ProfileSourceOverride = "override"
)

// PredicateProfile is the current version of one predicate's learned ontology (table
// predicate_profiles, migration 0022). KindSpec stays static and the registry immutable
// (kinds.go); cardinality is DATA, read by the authority as an input.
type PredicateProfile struct {
	NamespaceID string
	Kind        string
	Predicate   string
	Cardinality string
	Source      string
	SingleVotes int
	MultiVotes  int
	// EvidenceAssessmentIDs is the provenance: the assessments whose verdicts voted.
	// Each id votes at most once.
	EvidenceAssessmentIDs []string
	AliasOf               string
	SetBy                 string
	Reason                string
	SystemFrom            time.Time
}

// ProfileVote is one assessment's contribution to a predicate's votes.
type ProfileVote struct {
	AssessmentID string
	Single       int
	Multi        int
}

// ApplyProfileVote is the pure learning rule (A1.4). It returns the next version of cur
// and whether anything changed. A vote whose assessment id already voted is ignored —
// replaying a stored assessment never double-counts. The learned cardinality is
// `single` (or `multi`) once that side has at least minVotes votes and the other side
// none; a split vote is `unknown`, so the judge keeps being asked rather than the
// authority closing values on a coin flip. A declared or override profile keeps its
// cardinality whatever the votes say — config (v1's only override channel, A1-Q5) wins —
// but still records the votes, so an operator can see what the judge thinks.
func ApplyProfileVote(cur PredicateProfile, v ProfileVote, minVotes int) (PredicateProfile, bool) {
	if minVotes <= 0 {
		minVotes = 3
	}
	if v.AssessmentID == "" || (v.Single == 0 && v.Multi == 0) {
		return cur, false
	}
	for _, id := range cur.EvidenceAssessmentIDs {
		if id == v.AssessmentID {
			return cur, false
		}
	}
	next := cur
	next.EvidenceAssessmentIDs = append(append([]string(nil), cur.EvidenceAssessmentIDs...), v.AssessmentID)
	next.SingleVotes += v.Single
	next.MultiVotes += v.Multi
	if next.Source == "" {
		next.Source = ProfileSourceLearned
	}
	if next.Source == ProfileSourceLearned {
		next.Cardinality = LearnedCardinality(next.SingleVotes, next.MultiVotes, minVotes)
	}
	if next.Cardinality == "" {
		next.Cardinality = CardinalityUnknown
	}
	return next, true
}

// LearnedCardinality maps vote counts to a cardinality (A1.4 vote threshold).
func LearnedCardinality(single, multi, minVotes int) string {
	switch {
	case single >= minVotes && multi == 0:
		return CardinalitySingle
	case multi >= minVotes && single == 0:
		return CardinalityMulti
	default:
		return CardinalityUnknown
	}
}

// ResolutionInputs is what an InputResolutionAuthority derives from besides the items
// of the key (A1.2). Every field is stored data — the derivation stays a pure function
// of rows, so a recompute from the same rows in any order gives the same chain.
type ResolutionInputs struct {
	// Cardinality of the key's predicate: single | multi | unknown (predicate_profiles).
	Cardinality string
	// Assessments are the stored verdicts citing any item of the key.
	Assessments []Assessment
	// Related are the OTHER side of cross-key verdicts: items of another key that an
	// assessment paired with an item of this one.
	Related []KnowledgeItem
}

// ItemInterval is one item's derived valid-time interval in its key's chain.
type ItemInterval struct {
	ItemID KnowledgeItemID
	// From/To bound [From, To); nil = unbounded on that side. Empty is true when the
	// item was overtaken at its own start (two values for the same instant).
	From  *time.Time
	To    *time.Time
	Empty bool
	// FromInferred: the lower bound is the assertion time, not a stated date.
	FromInferred bool
	// SupersededBy is the item that closed this one ("" = open, or ended by its own
	// stated end). It may be an item of another key (a cross-key contradiction) or an
	// erased sentinel.
	SupersededBy KnowledgeItemID
	// Current: the item holds now — open, not a negation, not a sentinel.
	Current bool
}

// ResolutionOutcome is the result of a derivation.
type ResolutionOutcome struct {
	Winner *KnowledgeItem
	Reason string
	// Chain is every item of the key with its derived interval, in chain order.
	Chain []ItemInterval
}

// InputResolutionAuthority is a ResolutionAuthority that also takes stored inputs
// (A1.2). It must stay PURE and ORDER-INDEPENDENT (memo §13): the same items and inputs
// in any order give the same outcome. A store type-asserts for it and widens its read.
type InputResolutionAuthority interface {
	ResolutionAuthority
	ResolveWith(items []KnowledgeItem, in ResolutionInputs) ResolutionOutcome
	// ProfilePredicate names the predicate whose profile supplies the key's
	// Cardinality ("" = none).
	ProfilePredicate(items []KnowledgeItem) string
}

// SentinelOnEraseAuthority is an authority whose chains must stay closed across an
// erasure (ADR-0137 Q5; ADR-0136 A1-Q4): before deleting, the store derives every
// affected key, and for each surviving item an erased item had CLOSED (at instant at) it
// inserts the content-free sentinel this names (ok=false: leave nothing).
type SentinelOnEraseAuthority interface {
	ErasedSentinel(erased, closed KnowledgeItem, at time.Time) (sentinel KnowledgeItem, ok bool)
}

// AssessmentStore is the port of the Assessment table. The postgres knowledge store
// implements it; the kernel reaches it through authz.EnforcingKnowledgeWriter.
type AssessmentStore interface {
	// GetAssessment returns the stored assessment with this id, or (nil, nil).
	GetAssessment(ctx context.Context, id string) (*Assessment, error)
	// PutAssessment stores a (idempotent by id) and, in the same transaction, re-derives
	// every resolution key its input items belong to. inserted=false when the id
	// already existed (a replay).
	PutAssessment(ctx context.Context, a Assessment) (inserted bool, err error)
}

// PredicateProfileStore is the port of predicate_profiles.
type PredicateProfileStore interface {
	// PredicateProfile returns the current profile; a missing one reads as
	// {Cardinality: unknown, Source: learned} with no votes.
	PredicateProfile(ctx context.Context, namespace, kind, predicate string) (PredicateProfile, error)
	// VoteProfile applies one assessment's votes (ApplyProfileVote) and versions the row
	// when it changed. cardinalityChanged reports a changed cardinality — the caller
	// then re-derives the predicate's keys.
	VoteProfile(ctx context.Context, namespace, kind, predicate string, v ProfileVote, minVotes int) (cardinalityChanged bool, err error)
	// SeedDeclaredProfiles makes the declared map the current declared profiles: a
	// predicate in the map gets source=declared with its cardinality; a declared row
	// no longer in the map reverts to learned (votes kept). It returns the predicates
	// whose cardinality changed.
	SeedDeclaredProfiles(ctx context.Context, namespace, kind string, declared map[string]string, minVotes int, setBy string) (changed []string, err error)
	// RederivePredicate re-derives every key of kind whose items carry predicate (at
	// most limit keys; 0 = all). Returns the keys re-derived.
	RederivePredicate(ctx context.Context, namespace, kind, predicate string, limit int) (int, error)
}

// ItemEmbedding is one row of the item embedding projection (knowledge_item_embeddings,
// migration 0022; the ADR-0107 pattern for items).
type ItemEmbedding struct {
	ItemID       KnowledgeItemID
	ModelID      string
	ModelVersion string
	Vector       []float32
}

// ExtractionMarker records that a document's content (ContentHash) was run through an
// extractor (knowledge_extractions, migration 0022), so a re-ingest of unchanged
// content spends no second LLM call.
//
// ADR-0136 A2.4 (migration 0024): the marker also names the extractor VERSION that ran
// (`<prompt id>/<generator model>`) and how much of the source it covered, counted in
// the extractor's passages (PassagesCovered of PassagesTotal). A marker of another
// version, a legacy marker (version ""), or a partial one (covered < total) is not
// "done" — see ExtractionMarker.Complete.
type ExtractionMarker struct {
	NamespaceID      string
	Extractor        string
	DocumentID       string
	ContentHash      string
	Items            int
	ExtractorVersion string
	PassagesCovered  int
	PassagesTotal    int
}

// Complete reports whether the marker records a full run of extractor version v. A
// legacy row (written before 0024: version "", no coverage) is never complete.
func (m *ExtractionMarker) Complete(version string) bool {
	return m != nil && m.ExtractorVersion != "" && m.ExtractorVersion == version &&
		m.PassagesCovered >= m.PassagesTotal
}

// ItemEmbeddingWriter writes the item embedding projection.
type ItemEmbeddingWriter interface {
	PutItemEmbeddings(ctx context.Context, embs []ItemEmbedding) error
}

// ExtractionMarkerStore reads and writes knowledge_extractions.
type ExtractionMarkerStore interface {
	// ExtractionMarker returns the marker of this content, or nil when there is none.
	ExtractionMarker(ctx context.Context, namespace, extractor, documentID, contentHash string) (*ExtractionMarker, error)
	// MarkExtracted upserts the marker: a later run of the same content (a resumed
	// partial run, or a new extractor version) replaces version, coverage and count.
	MarkExtracted(ctx context.Context, m ExtractionMarker) error
}

// FactSubstrateStore is the RAW substrate the ADR-0136 A1 fact path is built on
// (postgres.PgKnowledgeStore). Only kernel.NewMemoryStack holds it, and it wraps it in
// authz.EnforcingKnowledgeWriter before anything else sees it (AGENTS.md §2.1).
type FactSubstrateStore interface {
	KnowledgeStore
	AssessmentStore
	PredicateProfileStore
	ItemEmbeddingWriter
	ExtractionMarkerStore
}

// FactSubstrateWriter is the ENFORCED write side the fact extractor receives: items go
// through the classification chokepoint (PutClassifiedItem returns the item as STORED —
// final classification, re-keyed identity), the rest is kernel-authored bookkeeping
// over items that already passed it.
type FactSubstrateWriter interface {
	PutClassifiedItem(ctx context.Context, item KnowledgeItem) (stored KnowledgeItem, inserted bool, err error)
	AssessmentStore
	PredicateProfileStore
	ItemEmbeddingWriter
	ExtractionMarkerStore
}
