package domain

import "strings"

// Procedure is an INDUCED routine — "how this kind of work has gone here" (ADR-0094).
//
// It is descriptive, not normative. A Skill (ADR-0046) is authored by a human and says
// how something SHOULD be done; a Procedure is distilled from what actually happened and
// says how it HAS gone. Where both match, the skill is normative and the procedure is
// evidence about it — including, usefully, evidence that the documented way keeps
// failing. That boundary is authorship, and it is why the two are separate types
// rather than one with a flag.
type Procedure struct {
	ID string
	// Trigger is the ADR-0049 D7 situation projection — goal shape + abstracted entity
	// roles + environment kind. It is the EMBEDDING SUBJECT: you retrieve by situation
	// and act on steps. Specific entity ids are excluded, exactly as D7 excludes them,
	// because embedding ids makes every record unique and breaks similarity.
	Trigger string
	Steps   []ProcedureStep
	// KnownFailureModes links precedents observed while following this routine.
	KnownFailureModes []string
	// SourceExperiences is provenance, and the reason ADR-0095 D5's link table exists:
	// it makes "was this induced across a closed-tag boundary?" a query rather than a
	// belief (ADR-0095 D9).
	SourceExperiences []string
	// ContributingAgents is ATTRIBUTION ONLY and is never read at routing time. See
	// ProcedureStep.RequiredCapabilities for why.
	ContributingAgents []string
	// Tags is the inherited classification. A procedure is only ever induced from
	// classification-compatible sources, so this is their shared classification rather
	// than a union — refusing beats unioning (ADR-0095 D9).
	Tags []string
	// Confidence rises with corroboration and resists single runs (ADR-0094 D8).
	Confidence  float64
	SampleCount int
	Status      ProcedureStatus
	// SupersededBy names the procedure that replaced this one. Retained, not deleted:
	// a routine that stopped working is evidence, the same way a rejected arm is.
	SupersededBy string
}

// ProcedureStatus is the D4 lifecycle. Deprecation is a first-class state because
// memory that is only ever appended rots (arXiv:2508.06433).
type ProcedureStatus string

const (
	ProcedureActive     ProcedureStatus = "active"
	ProcedureDeprecated ProcedureStatus = "deprecated"
	ProcedureSuperseded ProcedureStatus = "superseded"
)

// ProcedureStep is one move in a routine, named by CAPABILITY rather than by agent.
//
// This is ADR-0094 D2, the load-bearing decision of that ADR. A stored routine that
// recorded which agent did each step would be a learned hardcoded routing table — the
// Zero-Hardcode Rule defeated by a database row instead of a Go switch, and not one of
// its three sanctioned exceptions. Naming capabilities keeps a retrieved procedure as
// planner INPUT: the Gatekeeper still filters and the Auctioneer still selects. It also
// means the routine survives fleet changes, because it says what is needed rather than
// who once did it.
type ProcedureStep struct {
	RequiredCapabilities []string
	Intent               string
	DependsOn            []int
}

// CapabilitySignature renders a procedure's ordered capability sequence — the SHAPE of
// the routine, independent of its prose. Two runs that used the same capabilities in
// the same order are the same routine even when the planner worded them differently,
// which is what lets induction group across surface form (ADR-0094 D3).
func (p Procedure) CapabilitySignature() string {
	parts := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		parts = append(parts, strings.Join(s.RequiredCapabilities, "+"))
	}
	return strings.Join(parts, ">")
}

// NamesAnyAgent reports whether any step leaks an agent identity into the routing
// surface. It must always be false: ContributingAgents is attribution and lives off the
// step. Exposed so the invariant is assertable rather than merely intended.
func (p Procedure) NamesAnyAgent(knownAgents map[string]bool) bool {
	for _, s := range p.Steps {
		for _, c := range s.RequiredCapabilities {
			if knownAgents[c] {
				return true
			}
		}
	}
	return false
}
