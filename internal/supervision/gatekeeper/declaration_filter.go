package gatekeeper

import "github.com/cambrian-sh/core/domain"

// PassesDeclaration performs the Layer 1 (Declaration) hard compatibility check.
// Rules:
//   - If manifest is nil → passes unconditionally (Provisional).
//   - If manifest is empty (no tools, no formats) → passes unconditionally.
//   - Every entry in task.RequiredFormats must appear in manifest.SupportedFormats.
func PassesDeclaration(manifest *domain.AgentManifest, task *domain.AuctionTask) bool {
	if manifest == nil {
		return true
	}
	if len(manifest.Tools) == 0 && len(manifest.SupportedFormats) == 0 {
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
