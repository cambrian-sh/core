package scope

import "strings"

// provenancePrefix marks kernel-stamped provenance tags. These are NEVER
// agent-supplied and are exempt from the controlled-vocabulary coinage check.
const provenancePrefix = "provenance:"

// Vocabulary is the deployment-time controlled set of classification tags
// (ADR-0034 D8). It is data-driven (operator-configured) but the EXISTENCE of the
// validation gate is a deterministic safety invariant — a Zero-Hardcode exception,
// same class as Reflexive Path / Omurilik routing. An empty vocabulary disables
// the coinage check (ForbiddenTags enforcement still applies).
type Vocabulary struct {
	set map[string]struct{}
}

// NewVocabulary builds a controlled vocabulary from the allowed classification tags.
func NewVocabulary(tags []string) *Vocabulary {
	set := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		set[t] = struct{}{}
	}
	return &Vocabulary{set: set}
}

// IsEmpty reports whether no vocabulary is configured (coinage check disabled).
func (v *Vocabulary) IsEmpty() bool { return v == nil || len(v.set) == 0 }

// Contains reports whether tag is a member of the controlled vocabulary.
func (v *Vocabulary) Contains(tag string) bool {
	if v == nil {
		return false
	}
	_, ok := v.set[tag]
	return ok
}

// IsProvenance reports whether tag is a kernel-stamped provenance tag (exempt from
// vocabulary membership — it is never agent-coined).
func IsProvenance(tag string) bool { return strings.HasPrefix(tag, provenancePrefix) }
