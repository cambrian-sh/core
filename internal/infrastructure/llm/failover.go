package llm

import (
	"errors"
	"slices"
)

// ErrNoHealthyModel is the terminal of the failover ladder (ADR-0042 D5). The
// Provider returns it — explicitly and logged — rather than ever handing back a
// nil/empty generator, the anti-pattern that produced the original incident.
var ErrNoHealthyModel = errors.New("llm provider: no healthy generator available")

// resolveModel walks the unified failover ladder and returns the chosen
// generator id, or ErrNoHealthyModel. It is pure decision logic: it gates on
// availability (the healthy predicate) and never expresses preference — that is
// supplied from outside (EFE candidates for agent steps / role config for system
// roles), keeping the Zero-Hardcode Rule intact.
//
// Ladder:
//  1. suggested model (the step's prior), if healthy
//  2. purpose preference, in order, first healthy
//  3. global default, if healthy
//  4. any healthy generator satisfying all capability hints
//  5. ErrNoHealthyModel
func resolveModel(
	suggestedID string,
	capabilityHints []string,
	preferenceIDs []string,
	allIDs []string,
	defaultID string,
	healthy func(string) bool,
	capIndex map[string][]string,
) (string, error) {
	// Membership gate for rungs 1–3: the breaker is optimistic about UNKNOWN
	// ids (unknown = no recorded failures = "healthy"), so without this a
	// suggestion, role or default naming a generator that is not in the table —
	// a typo, or one removed by a live reload — would resolve here and then
	// hard-error in Acquire's registry lookup, instead of falling down the
	// ladder as GeneratorForModel and the orphaned-role warning promise.
	known := func(id string) bool { return slices.Contains(allIDs, id) }
	// 1. suggested (prior)
	if suggestedID != "" && known(suggestedID) && healthy(suggestedID) {
		return suggestedID, nil
	}
	// 2. purpose preference, in order
	for _, id := range preferenceIDs {
		if id != "" && known(id) && healthy(id) {
			return id, nil
		}
	}
	// 3. global default
	if defaultID != "" && known(defaultID) && healthy(defaultID) {
		return defaultID, nil
	}
	// 4. any healthy generator matching capability hints
	for _, id := range capabilityCandidates(capabilityHints, allIDs, capIndex) {
		if healthy(id) {
			return id, nil
		}
	}
	// 5. terminal
	return "", ErrNoHealthyModel
}

// capabilityCandidates returns the ids eligible for the capability rung. With no
// hints, any generator qualifies (all ids). With hints, a generator must
// advertise every hinted capability.
func capabilityCandidates(hints, allIDs []string, capIndex map[string][]string) []string {
	if len(hints) == 0 {
		return allIDs
	}
	var out []string
	for _, id := range allIDs {
		if hasAllCapabilities(id, hints, capIndex) {
			out = append(out, id)
		}
	}
	return out
}

func hasAllCapabilities(id string, hints []string, capIndex map[string][]string) bool {
	for _, h := range hints {
		if !slices.Contains(capIndex[h], id) {
			return false
		}
	}
	return true
}

// firstChoice is the generator the ladder would pick for a call if every
// generator were healthy: the first KNOWN id of suggested → purpose preference →
// default, else the first capability candidate. It is the call's own model; any
// other generator serving the call is a fallback (ENG-453).
func firstChoice(suggestedID string, capabilityHints, preferenceIDs, allIDs []string, defaultID string, capIndex map[string][]string) string {
	id, _ := resolveModel(suggestedID, capabilityHints, preferenceIDs, allIDs, defaultID,
		func(string) bool { return true }, capIndex)
	return id
}

// fallbackPolicy decides which generator may serve a call its first choice could
// not (owner decision 2026-11-14, ENG-453): benchmark and pinned kernels never
// fall back; production falls back only to generators an operator marked
// equivalent (llm_provider.equivalent_generators). Immutable after construction.
type fallbackPolicy struct {
	pinned bool
	// equiv[a][b] ⇔ a and b share a configured group.
	equiv map[string]map[string]bool
}

func newFallbackPolicy(pinned bool, groups [][]string) fallbackPolicy {
	fp := fallbackPolicy{pinned: pinned, equiv: map[string]map[string]bool{}}
	for _, g := range groups {
		for _, a := range g {
			for _, b := range g {
				if a == "" || b == "" || a == b {
					continue
				}
				if fp.equiv[a] == nil {
					fp.equiv[a] = map[string]bool{}
				}
				fp.equiv[a][b] = true
			}
		}
	}
	return fp
}

// admits reports whether id may serve a call whose first choice is first. The
// first choice itself always may; with no first choice (nothing resolvable) there
// is nothing to fall back from, so nothing is refused here.
func (fp fallbackPolicy) admits(first, id string) bool {
	if first == "" || id == first {
		return true
	}
	if fp.pinned {
		return false
	}
	return fp.equiv[first][id]
}

// describe says, for an error, why nothing else served the call.
func (fp fallbackPolicy) describe(first string) string {
	if fp.pinned {
		return "llm_provider.pinned forbids any fallback"
	}
	if len(fp.equiv[first]) == 0 {
		return "no generator is marked equivalent to it (llm_provider.equivalent_generators)"
	}
	return "no generator marked equivalent to it is available"
}
