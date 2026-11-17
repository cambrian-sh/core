package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Row classification on typed rows (ADR-0158 D1).
//
// Events, observations and links carry a ROW classification beside the
// classification they reach through their evidence row, and an entity carries the
// row classification of the row that minted it (FirstSeenClassification, D4). It
// can only NARROW what a reader sees: a typed row is visible only when the reader
// passes the fence over its evidence tags AND the fence over (evidence tags ∪ row
// tags) (ADR-0158 D2, rendered in internal/authz). A plain union would not be
// narrow-only — required and any-of terms are monotone in the tag set — which is
// why the column is a second classification, never merged into the evidence's.
//
// The only producer that sets it is a mapping with its own classification
// (premium ingress studio, ADR-0158 D6 — still refused there until the last build
// stage, ENG-1041); every other producer writes it empty, and an empty row
// classification reads exactly as before.

// ErrRowClassificationRefused is returned (wrapped) when a typed row's
// classification is malformed or names a kernel-stamped tag.
var ErrRowClassificationRefused = errors.New("row classification refused")

// rowClassificationReservedPrefixes are the tag namespaces only the kernel
// stamps (ADR-0158 D1; ADR-0141 D10, ADR-0146 D5): owner tags, provenance stamps
// and ingress surface/transport tags. Under the narrow-only fence such a tag could
// only narrow, but refusing it keeps the column's meaning simple — a row
// classification is a producer's restriction, never an identity claim.
var rowClassificationReservedPrefixes = []string{OwnerTagPrefix, "provenance:", "ingress:"}

// NormalizeRowClassification validates a typed row's classification and returns
// it in canonical form: sorted, nil when empty (nil ≡ empty, ADR-0158 D1). It
// refuses a blank tag, a tag with surrounding whitespace, a duplicate, and a tag in
// a kernel-stamped namespace — the same shape rules ENG-1041 applies to a mapping
// spec, enforced again at the store because the store is the last writer. The
// deployment vocabulary check stays in premium (ENG-1041); the kernel does not
// know the vocabulary.
//
// The canonical order makes the stored value independent of the order a producer
// listed its tags in, so two writes of the same restriction store the same array.
// The input is never mutated.
func NormalizeRowClassification(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("%w: a blank tag", ErrRowClassificationRefused)
		}
		if strings.TrimSpace(t) != t {
			return nil, fmt.Errorf("%w: tag %q has surrounding whitespace", ErrRowClassificationRefused, t)
		}
		for _, p := range rowClassificationReservedPrefixes {
			if strings.HasPrefix(t, p) {
				return nil, fmt.Errorf("%w: tag %q is in the kernel-stamped %q namespace", ErrRowClassificationRefused, t, p)
			}
		}
		if seen[t] {
			return nil, fmt.Errorf("%w: tag %q is listed twice", ErrRowClassificationRefused, t)
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}
