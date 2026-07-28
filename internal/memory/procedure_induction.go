package memory

import (
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Candidate detection for the ADR-0094 procedural tier — DETERMINISTIC, no LLM.
//
// D3 splits induction in three: cluster deterministically, threshold on recurrence, and
// only then spend ONE LLM call per candidate to naturalise the prose. The LLM never
// decides WHETHER something is a routine, only how to phrase one this pass already
// identified. That is what keeps the tier inside the kernel's "no LLM at chunk
// granularity" rule — cost scales with the number of distinct routines, which saturates,
// rather than with traffic, which does not.

// ProcedureCandidate is a cluster of episodes that took the same shape.
type ProcedureCandidate struct {
	// Signature is the capability SET shared by every member — deduplicated and
	// sorted, with "a+b" step combos split into their parts.
	//
	// It was the ordered sequence until 2026-07-28, when measurement showed the
	// order was noise: one benchmark family produced ["file_read","file_read"],
	// ["file_read","file_read","file_read"], ["file_read+general_purpose","file_read"]
	// and ["general_purpose","general_purpose+file_read","file_read"] across runs of
	// THE SAME task, because the planner varies step count and tag grouping run to
	// run. Keying on the sequence made every run its own routine.
	Signature string
	// Sequence is a representative ORDERED capability sequence for the cluster (the
	// first member's, in stable id order). The signature answers "is this the same
	// routine?"; the sequence answers "what does it do?" and is what becomes the
	// procedure's steps. Collapsing to a set for clustering must not also collapse
	// the body, or every routine degenerates to one step per distinct capability.
	Sequence []string
	// Trigger is the representative situation projection (the alphabetically first, so
	// the choice is stable across runs rather than dependent on map iteration order).
	Trigger string
	// ExperienceIDs are the episodes that produced it — provenance, and what makes the
	// classification-compatibility check of ADR-0095 D9 a query rather than a belief.
	ExperienceIDs []string
	// Tags is the classification shared by every member. Induction across a mixed set
	// is REFUSED, not unioned, so this is genuinely shared rather than merged.
	Tags        []string
	SampleCount int
}

// EpisodeShape is one outcome record reduced to what induction needs.
type EpisodeShape struct {
	ExperienceID string
	Trigger      string
	// RawTrigger is the UNNORMALIZED projection. Trigger is normalized for CLUSTERING
	// (paths, filenames, digits and stopwords stripped), which deliberately makes
	// "create alpha.md" and "create beta.md" the same situation. That is right for
	// grouping and wrong for counting: without the raw form a bucket cannot tell eight
	// reruns of ONE task from one run of eight different ones.
	RawTrigger string
	Capabilities []string // ordered, one entry per step
	Succeeded    bool
	Tags         []string
}

// distinctKeyOf identifies the concrete SITUATION an episode occupied.
//
// Prefers the raw projection. When a record carries none — an older scene, or a
// caller that never set it — it falls back to the EXPERIENCE ID, which is unique per
// episode and therefore degrades to exactly the previous row-counting behaviour.
//
// Falling back to the normalized Trigger would be wrong in the other direction:
// normalization deliberately makes distinct situations share a trigger, so it would
// collapse genuine variants into one observation and suppress promotions that should
// happen.
func distinctKeyOf(e EpisodeShape) string {
	if e.RawTrigger != "" {
		return e.RawTrigger
	}
	return e.ExperienceID
}

// InduceCandidates clusters successful episodes into procedure candidates.
//
// Clustering keys on BOTH the capability-sequence shape and the trigger, because either
// alone over-groups: similar situations solved by different routines are not one
// procedure, and identical capability sequences in unrelated situations are not either.
//
// Only successes are inducible. A routine is a description of what works; failures are
// already represented as precedents (ADR-0049 A2.3) and inducing a procedure from them
// would produce a reusable recipe for failing.
//
// minSamples is the D3 promotion threshold: below it a cluster is a coincidence, not a
// routine.
func InduceCandidates(episodes []EpisodeShape, minSamples int) []ProcedureCandidate {
	if minSamples < 1 {
		minSamples = 1
	}
	type bucket struct {
		trigger string
		ids     []string
		tags    []string
		seq     []string // representative ordered sequence (see ProcedureCandidate.Sequence)
		ok      bool     // classification-compatible so far
		// distinct situations seen in this bucket, keyed by raw projection. A routine
		// observed with eight DIFFERENT targets is strong evidence; the same target
		// eight times is one observation, and counting it eight times inflates
		// confidence in precisely the shape that should not earn it.
		seen map[string]struct{}
	}
	buckets := map[string]*bucket{}

	for _, e := range episodes {
		if !e.Succeeded {
			continue
		}
		sig := capabilitySignature(e.Capabilities)
		if sig == "" {
			continue // a plan with no capability contract has no recognisable shape
		}
		key := sig + "\x00" + normalizeTrigger(e.Trigger)
		b, seen := buckets[key]
		if !seen {
			buckets[key] = &bucket{
				trigger: e.Trigger,
				ids:     []string{e.ExperienceID},
				tags:    append([]string(nil), e.Tags...),
				seq:     append([]string(nil), e.Capabilities...),
				ok:      true,
				seen:    map[string]struct{}{distinctKeyOf(e): {}},
			}
			continue
		}
		// ADR-0095 D9: a derived artifact inherits its sources' restrictions, and
		// derivation across a boundary is REFUSED rather than unioned — a union
		// produces something only slightly restricted that still reads as governed.
		if !sameClassification(b.tags, e.Tags) {
			b.ok = false
			continue
		}
		// A rerun of a situation already in this bucket is provenance, not evidence:
		// its id is recorded so the routine still points at every episode it came
		// from, but it does not advance the promotion count.
		dk := distinctKeyOf(e)
		if _, dup := b.seen[dk]; dup {
			b.ids = append(b.ids, e.ExperienceID)
			continue
		}
		b.seen[dk] = struct{}{}
		// Stable representatives: tie to the lowest experience id so the chosen
		// trigger and sequence do not depend on map/slice iteration order.
		if e.ExperienceID < b.ids[0] {
			b.seq = append([]string(nil), e.Capabilities...)
		}
		b.ids = append(b.ids, e.ExperienceID)
		if e.Trigger < b.trigger {
			b.trigger = e.Trigger // stable representative
		}
	}

	out := make([]ProcedureCandidate, 0, len(buckets))
	for key, b := range buckets {
		// Promotion counts DISTINCT situations, not rows. Re-running one task twice
		// used to satisfy min_samples=2 on its own.
		if !b.ok || len(b.seen) < minSamples {
			continue
		}
		sig := key
		if i := strings.IndexByte(key, 0); i >= 0 {
			sig = key[:i]
		}
		sort.Strings(b.ids)
		out = append(out, ProcedureCandidate{
			Signature:     sig,
			Sequence:      b.seq,
			Trigger:       b.trigger,
			ExperienceIDs: b.ids,
			Tags:          b.tags,
			SampleCount:   len(b.seen),
		})
	}
	// Deterministic order: the same corpus must yield the same candidates in the same
	// order on every run, or a batch pass produces different procedures each night.
	sort.Slice(out, func(i, j int) bool {
		if out[i].SampleCount != out[j].SampleCount {
			return out[i].SampleCount > out[j].SampleCount
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

// capabilitySignature renders a capability SET: "a+b" step combos are split, entries
// deduplicated, sorted, and joined. Capabilities are already canonical (ADR-0067
// NormalizeCapability), so no case folding is needed here.
//
// Set, not sequence, since 2026-07-28. The planner does not decompose the same request
// into the same steps twice — measured across nine runs of one benchmark family, step
// count varied 1..3 and tags regrouped (`file_read+general_purpose` one run, two separate
// steps the next). Keying on order meant a family never accumulated two samples and no
// routine could ever be induced. The ordered sequence survives on
// ProcedureCandidate.Sequence for the procedure body.
func capabilitySignature(caps []string) string {
	seen := map[string]struct{}{}
	for _, c := range caps {
		for _, part := range strings.Split(c, "+") {
			if part = strings.TrimSpace(part); part != "" {
				seen[part] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return ""
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// triggerStopwords are dropped before keying: articles, prepositions and the filler
// verbs that survive every phrasing. They carry no situational information, and leaving
// them in means "create X in Y" and "create X" are different situations.
var triggerStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "of": {}, "to": {}, "in": {},
	"on": {}, "at": {}, "for": {}, "with": {}, "from": {}, "into": {}, "it": {},
	"its": {}, "is": {}, "be": {}, "goal": {}, "engages": {}, "then": {}, "that": {},
	"this": {}, "by": {}, "as": {},
}

// normalizeTrigger reduces a situation projection to a comparison key.
//
// It used to lowercase and collapse whitespace, nothing more, on the reasoning that
// anything cleverer would make clustering non-deterministic. That reasoning conflated
// "clever" with "non-deterministic" — everything below is a pure function of the input
// — and the crude version could not cluster real data: 19 stored scenes produced 19
// distinct triggers, because the projection embeds the planner's free-text goal, which
// names the concrete file, the per-run sandbox path, and any quoted literal
// ("Create and verify notes/alpha.md in runs/rt_diag6/workspace"). Every run minted a
// new situation and induction could never reach two samples.
//
// So: drop the volatile specifics (paths, filenames, quoted literals, digits), drop
// stopwords, stem the handful of suffixes that make "verify"/"verification" look like
// different situations, then compare as a SORTED SET of the remaining tokens — word
// order in a prose goal is phrasing, not situation.
//
// This is still exact-match clustering on a deterministic key. It is deliberately NOT
// embedding similarity: a threshold would make two runs of the same corpus produce
// different routines depending on what else was in the batch.
func normalizeTrigger(t string) string {
	seen := map[string]struct{}{}
	for _, raw := range strings.Fields(strings.ToLower(t)) {
		tok := normalizeTriggerToken(raw)
		if tok == "" {
			continue
		}
		if _, drop := triggerStopwords[tok]; drop {
			continue
		}
		seen[tok] = struct{}{}
	}
	if len(seen) == 0 {
		return ""
	}
	out := make([]string, 0, len(seen))
	for tok := range seen {
		out = append(out, tok)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// normalizeTriggerToken reduces one token, returning "" for tokens that carry no
// situational information (paths, filenames, quoted literals, bare numbers).
func normalizeTriggerToken(tok string) string {
	tok = strings.Trim(tok, ".,;:!?()[]{}'\"`|")
	if tok == "" {
		return ""
	}
	// A path or a filename names the concrete THING acted on, which is exactly what
	// differs between two runs of the same routine.
	if strings.ContainsAny(tok, "/\\") {
		return ""
	}
	if i := strings.LastIndexByte(tok, '.'); i > 0 && i < len(tok)-1 {
		return "" // "alpha.md", "one.sum.md" — a filename
	}
	// Counts ("2 directory, 2 file") vary with how the plan decomposed, not with the
	// situation: the same task engaged 1 directory one run and 5 the next.
	allDigits := true
	for _, r := range tok {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return ""
	}
	return stemTriggerToken(tok)
}

// stemTriggerToken strips the small set of suffixes that make one situation look like
// several ("create"/"creation", "verify"/"verification", "summary"/"summarize"). A crude
// hand-rolled stemmer on purpose: a full Porter implementation would be a dependency and
// a lot of behaviour for a key that only needs morphological variants to collide.
func stemTriggerToken(tok string) string {
	for _, suffix := range []string{"ication", "ications", "ation", "ations", "ing", "ies", "ed", "es", "s"} {
		if len(tok) > len(suffix)+2 && strings.HasSuffix(tok, suffix) {
			tok = tok[:len(tok)-len(suffix)]
			break
		}
	}
	// "verif"/"verify", "summar"/"summary" — normalise the trailing y/i left behind.
	return strings.TrimRight(tok, "yi")
}

// sameClassification reports whether two tag sets are identical as sets. Set equality,
// not overlap: "compatible" must mean "the same restrictions apply", or induction
// quietly widens access for the members with fewer tags.
func sameClassification(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, t := range a {
		seen[t]++
	}
	for _, t := range b {
		seen[t]--
		if seen[t] < 0 {
			return false
		}
	}
	return true
}

// ToProcedure turns a candidate into the record ADR-0094 D1 describes. The steps carry
// CAPABILITIES only (D2): naming the agents that happened to run them would make the
// routine a learned routing table and defeat the auction.
func (c ProcedureCandidate) ToProcedure(id string, intents []string) domain.Procedure {
	// The SEQUENCE, not the signature: the signature is now an unordered set used only
	// as a cluster key, and building steps from it would turn every routine into one
	// step per distinct capability regardless of what the plans actually did.
	caps := c.Sequence
	if len(caps) == 0 {
		caps = strings.Split(c.Signature, ",")
	}
	steps := make([]domain.ProcedureStep, 0, len(caps))
	for i, capability := range caps {
		intent := ""
		if i < len(intents) {
			intent = intents[i]
		}
		steps = append(steps, domain.ProcedureStep{
			RequiredCapabilities: strings.Split(capability, "+"),
			Intent:               intent,
		})
	}
	return domain.Procedure{
		ID:                id,
		Trigger:           c.Trigger,
		Steps:             steps,
		SourceExperiences: c.ExperienceIDs,
		Tags:              c.Tags,
		SampleCount:       c.SampleCount,
		// Seeded from the induction evidence, NOT left at zero.
		//
		// InduceCandidates only clusters SUCCESSES, so a routine promoted from N
		// episodes has an observed record of N-for-N. Leaving Confidence at the zero
		// value discarded that and made the routine indistinguishable from one that had
		// just failed everything — so its very first successful use nudged it to 0.15,
		// tripped the deprecation floor, and retired a routine that had never failed.
		// Induction must hand its evidence to the lifecycle, not drop it at the door.
		Confidence: 1.0,
		Status:     domain.ProcedureActive,
	}
}
