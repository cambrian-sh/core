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
