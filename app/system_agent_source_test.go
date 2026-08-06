package app

import "testing"

// A plugin directory must not be a privilege-escalation seam.
//
// NewFilesystemAgentSource confers nothing on purpose: a directory is not an
// audit trail, and a plugin that could privilege anything it dropped on disk
// would be exactly that. The system-conferring constructor keeps the property by
// requiring the IDs to be NAMED in the compiled-in plugin's own source.
func TestOnlyNamedAgentsGetSystemStatusFromAPluginDirectory(t *testing.T) {
	s := NewSystemFilesystemAgentSource("/plugins/ingressstudio/agents", "source_discovery_agent")
	if !s.isSystemAgent("source_discovery_agent") {
		t.Error("the named agent was not granted system status")
	}
	// The whole point: a second agent sitting in the same directory is ordinary.
	if s.isSystemAgent("something_else_in_the_same_dir") {
		t.Error("an unnamed agent in the directory was privileged — the directory must not confer status")
	}
	// And the plain constructor still confers nothing at all.
	if NewFilesystemAgentSource("/plugins/x").isSystemAgent("source_discovery_agent") {
		t.Error("the plain plugin source conferred system status")
	}
}
