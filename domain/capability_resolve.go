package domain

import (
	"fmt"
	"sort"
	"strings"
)

// GeneralistCapability is the tag an agent declares to say "I can attempt work
// outside my specialisms". It is the last rung of the resolution ladder: a step
// whose requirements nothing declares is offered to generalists rather than
// killed outright. Declared by every reference agent today.
const GeneralistCapability = "general_purpose"

// Resolution tiers, recorded so a routing decision can say HOW it matched.
const (
	// TierExact — every required capability is declared verbatim (after
	// normalization) by at least one registered agent.
	TierExact = "exact"
	// TierAlias — at least one requirement matched only after an authored alias
	// was applied.
	TierAlias = "alias"
	// TierGeneralist — a requirement matched nothing at all, so the step falls
	// back to agents declaring GeneralistCapability.
	TierGeneralist = "generalist"
	// TierUnsatisfiable — nothing matched and no generalist is registered.
	TierUnsatisfiable = "unsatisfiable"
)

// CapabilityResolution is the outcome of resolving a step's declared
// requirements against the live capability vocabulary (ADR-0100 D5).
type CapabilityResolution struct {
	// Resolved is the capability set the L1 gate should enforce. On the
	// generalist tier it is {GeneralistCapability}; on the unsatisfiable tier it
	// is the original requirement set (so the gate rejects everything and the
	// failure is loud rather than silently unrouted).
	Resolved []string
	// Unmatched lists requirements no registered agent declares, in input order.
	Unmatched []string
	// Tier is one of TierExact / TierAlias / TierGeneralist / TierUnsatisfiable.
	Tier string
	// Vocabulary is the sorted live capability vocabulary, carried so an error
	// can tell the operator what the fleet actually offers.
	Vocabulary []string
}

// Satisfiable reports whether any agent can possibly clear the L1 gate.
func (r CapabilityResolution) Satisfiable() bool { return r.Tier != TierUnsatisfiable }

// NoCapabilityMatchError names the gap between what a step asked for and what
// the fleet declares. It exists so an unroutable step fails LOUDLY and
// diagnosably — the ADR-0100 D5 requirement — instead of surfacing as a generic
// "no candidates".
type NoCapabilityMatchError struct {
	TaskID     string
	Unmatched  []string
	Vocabulary []string
}

func (e *NoCapabilityMatchError) Error() string {
	vocab := "none — no agent declares any capability"
	if len(e.Vocabulary) > 0 {
		vocab = strings.Join(e.Vocabulary, ", ")
	}
	return fmt.Sprintf(
		"step %s requires capability %v which no registered agent declares, and no %s agent is available; registered vocabulary: [%s]",
		e.TaskID, e.Unmatched, GeneralistCapability, vocab)
}

// ResolveCapabilities walks the ADR-0100 D5 ladder:
//
//	NormalizeCapability → authored alias map → generalist tier → unsatisfiable
//
// There is deliberately NO embedding-nearest rung. ADR-0067 rejected fuzzy
// capability matching because `file-read`/`file-write` and `read`/`delete` are
// embedding-close and semantically opposite; a wrong merge silently misroutes,
// which is worse than failing. Synonyms belong in a reviewed data file.
//
// The function is pure: vocabulary and aliases are supplied by the caller, so
// nothing about the live fleet is hardcoded.
// Resolved capabilities are returned in the fleet's OWN declared spelling, never
// the normalized form. The L1 gate only normalizes when execution.canonical_vocab
// is on; with it off the comparison is verbatim, so handing back a normalized tag
// would gate out the very agent that declares it.
// agentSets is each live agent's declared capability set. Satisfiability is
// checked against those sets, NOT against their union: L1 requires ONE agent to
// declare EVERY required capability, so a conjunction that the fleet can satisfy
// collectively but no single agent can satisfy alone must still fall back.
// Measured 2026-07-29 on the orchestration probe: 2 of 3 tasks died `no_candidate`
// with every tag present in the vocabulary — union membership is not eligibility.
func ResolveCapabilities(required, vocabulary []string, aliases map[string]string, agentSets [][]string) CapabilityResolution {
	res := CapabilityResolution{Tier: TierExact, Vocabulary: dedupeDeclared(vocabulary)}
	if len(required) == 0 {
		return res
	}

	// normalized form → the spelling the fleet actually declares.
	declaredBy := make(map[string]string, len(vocabulary))
	for _, c := range vocabulary {
		if n := NormalizeCapability(c); n != "" {
			if _, dup := declaredBy[n]; !dup {
				declaredBy[n] = c
			}
		}
	}
	generalist, hasGeneralist := declaredBy[NormalizeCapability(GeneralistCapability)]

	// Normalize the authored map's KEYS too, so an operator writing
	// {"web_search": ...} still matches a planner emitting `web-search`.
	normAliases := make(map[string]string, len(aliases))
	for k, v := range aliases {
		if n := NormalizeCapability(k); n != "" {
			normAliases[n] = v
		}
	}

	resolved := make([]string, 0, len(required))
	usedAlias := false

	for _, req := range required {
		n := NormalizeCapability(req)
		if declared, ok := declaredBy[n]; ok {
			resolved = append(resolved, declared)
			continue
		}
		// Rung 2: authored alias. Both sides are normalized on lookup so the map is
		// forgiving about how an operator wrote it.
		if len(normAliases) > 0 {
			if target, ok := normAliases[n]; ok {
				if declared, known := declaredBy[NormalizeCapability(target)]; known {
					resolved = append(resolved, declared)
					usedAlias = true
					continue
				}
			}
		}
		res.Unmatched = append(res.Unmatched, req)
	}

	if len(res.Unmatched) > 0 {
		// Rung 3: a requirement nothing declares. Do NOT gate on a capability no
		// agent has — that admits nobody. Offer the step to generalists instead.
		if hasGeneralist {
			res.Tier = TierGeneralist
			res.Resolved = []string{generalist}
			return res
		}
		// Rung 4: nothing left to try. Keep the original requirements so the gate
		// admits nobody and the caller reports the gap.
		res.Tier = TierUnsatisfiable
		res.Resolved = required
		return res
	}

	// Every requirement is spelled by SOMEBODY — but eligibility needs one agent
	// to hold the whole set. Without this check a satisfiable-looking conjunction
	// gates every candidate out and the step dies with an empty slate.
	if !anyAgentSatisfies(resolved, agentSets) {
		if hasGeneralist {
			res.Tier = TierGeneralist
			res.Unmatched = required // the SET is unsatisfiable, not any single tag
			res.Resolved = []string{generalist}
			return res
		}
		res.Tier = TierUnsatisfiable
		res.Unmatched = required
		res.Resolved = required
		return res
	}

	if usedAlias {
		res.Tier = TierAlias
	}
	res.Resolved = resolved
	return res
}

// anyAgentSatisfies reports whether at least one agent declares every capability
// in required. No agent sets supplied ⇒ true (nothing to contradict; the caller
// simply has no per-agent view).
func anyAgentSatisfies(required []string, agentSets [][]string) bool {
	if len(agentSets) == 0 {
		return true
	}
	for _, set := range agentSets {
		have := make(map[string]struct{}, len(set))
		for _, c := range set {
			have[NormalizeCapability(c)] = struct{}{}
		}
		all := true
		for _, r := range required {
			if _, ok := have[NormalizeCapability(r)]; !ok {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// BuildCapabilityVocabulary collects the distinct capabilities the live fleet
// declares, in the fleet's own spelling, de-duplicated by normalized form.
func BuildCapabilityVocabulary(manifests []*AgentManifest) []string {
	all := make([]string, 0, len(manifests)*4)
	for _, m := range manifests {
		if m == nil {
			continue
		}
		all = append(all, m.Capabilities...)
	}
	return dedupeDeclared(all)
}

// dedupeDeclared de-duplicates by normalized form while preserving the declared
// spelling, sorted for stable logs and error messages.
func dedupeDeclared(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		n := NormalizeCapability(s)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
