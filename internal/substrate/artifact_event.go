package substrate

import (
	"encoding/json"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ArtifactDescriptor is the JSON shape an agent includes in its Handoff
// context under the "_artifacts" key to signal produced artifacts.
type ArtifactDescriptor struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

// ExtractArtifactDescriptors parses the _artifacts context key from a Handoff response.
// Returns nil if no artifacts were declared.
func ExtractArtifactDescriptors(resp *domain.Handoff) ([]ArtifactDescriptor, error) {
	if resp == nil || resp.Context == nil {
		return nil, nil
	}
	raw, ok := resp.Context["_artifacts"]
	if !ok || raw == "" {
		return nil, nil
	}
	var descs []ArtifactDescriptor
	if err := json.Unmarshal([]byte(raw), &descs); err != nil {
		return nil, fmt.Errorf("parse _artifacts: %w", err)
	}
	return descs, nil
}
