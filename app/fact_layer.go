package app

import "github.com/cambrian-sh/core/domain"

// factsAssessorModel names the judge of ADR-0136 A1 assessments: the generator id the
// "facts" role resolves to (llm_provider.roles.facts), else "default". It is part of
// every assessment's authority_version, so a different model is a different judge and
// never replays another model's verdict.
func factsAssessorModel(roles map[string]string) string {
	if id := roles[string(domain.PurposeFacts)]; id != "" {
		return id
	}
	return "default"
}
