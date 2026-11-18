package app

import (
	"context"
	"os"
	"path/filepath"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/mapper"
	"github.com/cambrian-sh/core/internal/storage"
)

// FilesystemAgentSource is an AgentSource (ADR-0075) that scans a directory for agents
// using the SAME discovery the built-in seeder uses (storage.DiscoverFilesystemAgents),
// and carries each agent's MANIFEST (PythonDeps/MemoryLimitMB/schemas) so it fully
// replaces — not merely approximates — the built-in scan. The kernel registers its own
// agents directory through the built-in (system-aware) variant; a plugin contributes its
// OWN directory via Registry.AddAgentSource:
//
//	r.AddAgentSource(app.NewFilesystemAgentSource("/opt/myplugin/agents"))
//
// It health-gates (ADR-0075): a discovered agent whose ExecPath does not resolve on disk
// is skipped — discovery ≠ availability.
type FilesystemAgentSource struct {
	dir           string
	isSystemAgent func(string) bool
	mapper        mapper.AgentMapper
	// skillTools is the skill index declarative profiles are validated against
	// (storage.DiscoverOptions.SkillTools); nil skips the skill checks.
	skillTools map[string][]string
	// composedRuntimeID registers the ADR-0145 D12 composed runtime under this id
	// (storage.DiscoverOptions.ComposedRuntimeID); "" ⇒ not registered.
	composedRuntimeID string
	// refused holds the ids the last DiscoverAgents refused to register
	// (storage.DiscoverOptions.OnRefused, ENG-2085); RefusedAgentIDs reads it.
	refused []string
}

// RefusedAgentIDs returns the ids the last DiscoverAgents found but refused to
// register (a manifest trust_tier that would raise privilege or is unknown,
// ENG-2085). registerAgentSources drops their earlier registrations.
func (s *FilesystemAgentSource) RefusedAgentIDs() []string { return s.refused }

// withComposedRuntime registers the composed-worker runtime (execution.workers.composed)
// under id beside the profile agents. Only the kernel's own source calls it.
func (s *FilesystemAgentSource) withComposedRuntime(id string) *FilesystemAgentSource {
	s.composedRuntimeID = id
	return s
}

// withSkillTools sets the skill index profiles in this directory are validated
// against (agent stack v2): every skill a profile lists must exist, and the tools
// it grants must sit inside the profile's allowlist.
func (s *FilesystemAgentSource) withSkillTools(skillTools map[string][]string) *FilesystemAgentSource {
	s.skillTools = skillTools
	return s
}

// NewFilesystemAgentSource builds a source over a PLUGIN-contributed directory. Its
// agents are always regular — the source's system predicate is always-false, so a
// plugin directory can never confer system privilege (that is the explicit AddSystemAgent
// grant).
func NewFilesystemAgentSource(dir string) *FilesystemAgentSource {
	return &FilesystemAgentSource{dir: dir, isSystemAgent: func(string) bool { return false }}
}

// NewSystemFilesystemAgentSource builds a plugin source that confers system status on
// the agents it NAMES, and on no others.
//
// The plain constructor above cannot confer it, deliberately: a directory is not an
// audit trail, and a plugin that could make anything it dropped on disk privileged
// would be a privilege-escalation seam. Naming the IDs at the call site keeps the
// property AddSystemAgent was written for — an explicit grant, visible in the
// compiled-in plugin's own source — while still discovering the agent by scanning,
// which is the only way a Python agent's manifest and exec path are found.
//
// Anything in `dir` that is not named here is a regular agent.
func NewSystemFilesystemAgentSource(dir string, systemIDs ...string) *FilesystemAgentSource {
	granted := make(map[string]bool, len(systemIDs))
	for _, id := range systemIDs {
		granted[id] = true
	}
	return &FilesystemAgentSource{dir: dir, isSystemAgent: func(id string) bool { return granted[id] }}
}

// newBuiltinFilesystemAgentSource builds the kernel's own agents-directory source. It is
// system-aware (domain.IsSystemAgent stamps the privileged organs), so it reproduces the
// pre-ADR-0075 built-in scan exactly — including manifests and system flags.
func newBuiltinFilesystemAgentSource(dir string) *FilesystemAgentSource {
	return &FilesystemAgentSource{dir: dir, isSystemAgent: domain.IsSystemAgent}
}

// Name identifies the source.
func (s *FilesystemAgentSource) Name() string { return "filesystem:" + s.dir }

// DiscoverAgents scans the directory and returns each agent's definition + manifest,
// skipping any whose exec path does not resolve on disk.
func (s *FilesystemAgentSource) DiscoverAgents(_ context.Context) ([]DiscoveredAgent, error) {
	s.refused = nil
	discovered, err := storage.DiscoverFilesystemAgentsWith(s.dir, s.isSystemAgent, storage.DiscoverOptions{
		SkillTools: s.skillTools, ComposedRuntimeID: s.composedRuntimeID,
		OnRefused: func(id string) { s.refused = append(s.refused, id) }})
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredAgent, 0, len(discovered))
	for _, da := range discovered {
		def := s.mapper.ToDomain(da.Agent)
		// Health gate: skip agents whose resolved exec path does not exist.
		if def.ExecPath != "" {
			resolved := filepath.Join(da.Agent.Dir, filepath.FromSlash(def.ExecPath))
			if _, statErr := os.Stat(resolved); statErr != nil {
				continue
			}
		}
		manifest := s.mapper.ManifestToDomain(da.Manifest)
		out = append(out, DiscoveredAgent{Definition: def, Manifest: &manifest})
	}
	return out, nil
}
