package gatekeeper

import "github.com/cambrian-sh/core/domain"

// PassesDeclaration performs the Layer 1 (Declaration) hard compatibility check.
// Rules:
//   - ROUTE-03 capability contract: if the task declares RequiredCapabilities,
//     they are a HARD gate — required ⊆ manifest.Capabilities, and a nil/empty
//     manifest can satisfy no capability, so the free pass below does NOT apply.
//     The task only carries RequiredCapabilities when the capability_contract
//     arm is on (populated at the Step→AuctionTask boundary), so the control arm
//     never reaches this branch and keeps byte-identical behavior.
//   - If manifest is nil → passes unconditionally (Provisional).
//   - If manifest is empty (no tools, no formats) → passes unconditionally.
//   - Every entry in task.RequiredFormats must appear in manifest.SupportedFormats.
// The canonical flag (ROUTE-04 / ADR-0067) applies deterministic capability
// normalization to BOTH sides of the subset check, so format/typo variance
// (`Web-Navigation` ≡ `web_navigation`) matches. It is byte-identical to the
// pre-ROUTE-04 verbatim check when false.
func PassesDeclaration(manifest *domain.AgentManifest, task *domain.AuctionTask, canonical bool) bool {
	norm := func(s string) string {
		if canonical {
			return domain.NormalizeCapability(s)
		}
		return s
	}
	// ROUTE-03: capability gate first — it is stricter than the free passes and
	// must veto an agent that cannot satisfy a declared capability requirement.
	if len(task.RequiredCapabilities) > 0 {
		if manifest == nil {
			return false
		}
		capSet := make(map[string]struct{}, len(manifest.Capabilities))
		for _, c := range manifest.Capabilities {
			capSet[norm(c)] = struct{}{}
		}
		for _, required := range task.RequiredCapabilities {
			if _, ok := capSet[norm(required)]; !ok {
				return false
			}
		}
		// Capabilities satisfied; still apply the format gate below (if any).
	}

	if manifest == nil {
		return true
	}
	if len(manifest.Tools) == 0 && len(manifest.SupportedFormats) == 0 {
		// No format/tool declarations to check. Capability requirements (if any)
		// were already enforced above.
		return true
	}

	formatSet := make(map[string]struct{}, len(manifest.SupportedFormats))
	for _, f := range manifest.SupportedFormats {
		formatSet[f] = struct{}{}
	}
	for _, required := range task.RequiredFormats {
		if _, ok := formatSet[required]; !ok {
			return false
		}
	}
	return true
}
