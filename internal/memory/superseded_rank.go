package memory

import (
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2158 (owner decision 2026-11-19: rank superseded lower). A document that is
// published in revisions is ingested once per revision — every revision is its own
// document (cambrian-premium cmd/cambrian-apply ingests superseded revisions on
// purpose, so a retrieval that returns one can be caught). The revisions share most
// of their text, so one question used to return the same chunk once per revision
// and the copies filled the window (readiness phase B: four SOP-001 revisions in
// the top 5).
//
// How "superseded" is known: the revision's own LABELS (ENG-2170, owner decision
// 2026-11-19). Core has no lifecycle state for separately-named revisions —
// ADR-0138 versions only a re-ingest of the SAME document id, and deletes the
// version it retires — so the writer marks the revision (cambrian-apply:
// `in-force` / `superseded`) and the marker reaches the chunk's metadata "labels"
// (domain.MetaLabels). It used to ride in the classification tags, where a write
// ceiling dropped it (and since ENG-2115 refused the write); labels are never
// classification, so the ceiling never sees them. A row written before labels
// existed still carries the marker in its tags, so the tags are read too — one
// more loop over a short list, and it keeps those corpora demoted without a
// re-ingest. Which markers mean "superseded" is configuration
// (execution.retrieval.superseded_markers), never a literal here.
//
// Why a multiplicative demotion, not a cap or an additive penalty:
//   - a per-document cap was rejected 2026-11-03 (it lost evidence);
//   - Score lives on different scales by the time this runs (RRF ≈ 1/61, the
//     Stage-A blend ≈ 0.5, section/anchor promotion 1 + cosine), so a fixed
//     additive penalty means something different on each; a factor is scale-free
//     and preserves the order among superseded rows;
//   - a demoted row is never dropped (the relevance floor tests RawScore, which
//     this does not touch), so an explicit ask still finds an old revision: a
//     query only the old revision matches still ranks it, and a read filter that
//     selects superseded rows scales every candidate alike, leaving their order as
//     it was.

// supersededDemotion is the configured demotion; the zero value is off.
type supersededDemotion struct {
	tags   map[string]bool // lower-cased markers, matched against labels and tags
	factor float64         // Score multiplier for a row carrying one, in (0, 1)
}

func (d supersededDemotion) active() bool { return len(d.tags) > 0 && d.factor > 0 && d.factor < 1 }

// EnableSupersededDemotion multiplies the score of every candidate whose labels (or,
// for rows written before labels, tags) carry one of markers by factor before the
// final assembly (execution.retrieval.superseded_markers / superseded_rank_factor). No markers, or a factor outside (0, 1),
// leaves ranking unchanged.
func (q *QueryService) EnableSupersededDemotion(markers []string, factor float64) {
	if q == nil {
		return
	}
	d := supersededDemotion{factor: factor}
	for _, m := range markers {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			if d.tags == nil {
				d.tags = make(map[string]bool, len(markers))
			}
			d.tags[m] = true
		}
	}
	if !d.active() {
		d = supersededDemotion{}
	}
	q.superseded = d
}

// isSuperseded reports whether a document's labels — or, for a row written before
// labels existed (ENG-2170), its tags — carry a configured marker.
func (d supersededDemotion) isSuperseded(doc domain.Document) bool {
	for _, l := range domain.LabelsOf(doc.Metadata) {
		if d.tags[strings.ToLower(l)] {
			return true
		}
	}
	for _, t := range docTags(doc.Metadata) {
		if d.tags[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

// demote scales the score of every superseded row and re-sorts by score, stably, so
// rows with equal scores keep the order the earlier stages gave them. It returns
// the input unchanged when the demotion is off or nothing is superseded.
//
// A superseded row whose text is also the text of a row that is NOT superseded (its
// successor's copy of the same chunk) is in addition held strictly below that copy:
// its score becomes factor × min(own, successor's). Identical texts do not score
// alike — the lexical lane breaks their tie by rank, and Hebbian/graph signals differ
// per chunk; the readiness corpus showed a superseded copy at twice its successor's
// score — so the factor alone could not promise "below its successor". Text identity
// is normalizedTextHashable's (whitespace-collapsed, lower-cased), the identity
// decomposition's dedup already uses.
func (d supersededDemotion) demote(results []domain.SearchResult) []domain.SearchResult {
	if !d.active() || len(results) == 0 {
		return results
	}
	superseded := make([]bool, len(results))
	current := map[string]float64{} // normalized text → best score among non-superseded rows
	found := false
	for i := range results {
		if d.isSuperseded(results[i].Document) {
			superseded[i], found = true, true
			continue
		}
		if t := normalizedTextHashable(results[i].Document.Text); t != "" {
			if s, ok := current[t]; !ok || results[i].Score > s {
				current[t] = results[i].Score
			}
		}
	}
	if !found {
		return results
	}
	for i := range results {
		if !superseded[i] {
			continue
		}
		s := results[i].Score
		if succ, ok := current[normalizedTextHashable(results[i].Document.Text)]; ok && succ < s {
			s = succ
		}
		results[i].Score = s * d.factor
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results
}
