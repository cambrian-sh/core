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
}

// NewFilesystemAgentSource builds a source over a PLUGIN-contributed directory. Its
// agents are always regular — the source's system predicate is always-false, so a
// plugin directory can never confer system privilege (that is the explicit AddSystemAgent
// grant).
func NewFilesystemAgentSource(dir string) *FilesystemAgentSource {
	return &FilesystemAgentSource{dir: dir, isSystemAgent: func(string) bool { return false }}
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
	discovered, err := storage.DiscoverFilesystemAgents(s.dir, s.isSystemAgent)
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
