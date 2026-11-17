package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentprofile"
)

// Declarative agent profiles (agent stack v2, owner directive
// decisions/agent-stack-v2-mcp-tools-and-profiles.md D3; format and the part-B
// contract in agents/profiles/README.md).
//
// A profile is one specialist written as data, `<agentsDir>/profiles/<name>.yaml`,
// run by the ONE runtime `<agentsDir>/worker_agent.py`. Discovery registers one
// agent per valid profile: id = the profile name, exec path = the runtime, and
// per-agent launch arguments `--profile <name> --agent-id <id>`. The runtime file
// itself is never an agent: it would run the default profile a second time under
// another id.
//
// Validation is the SDK's (internal/agentprofile is its parity-tested port), so
// the kernel registers exactly the profiles the runtime accepts. An invalid
// profile is a loud boot ERROR and is skipped; it never stops the kernel or hides
// the other profiles.

// ProfileRuntimeFile is the runtime every profile agent runs, relative to the
// agents directory.
const ProfileRuntimeFile = "worker_agent.py"

// ProfilesDirName is the profiles directory under the agents directory.
const ProfilesDirName = "profiles"

// DiscoverOptions tunes DiscoverFilesystemAgentsWith.
type DiscoverOptions struct {
	// SkillTools is the kernel's skill index (skill name → the tools its SKILL.md
	// grants) that profiles are validated against: every listed skill must exist
	// and its tools must sit inside the profile's allowlist. nil skips the two
	// skill checks (the SDK's skills_dir=None); the composition root always passes
	// the index of the skills directory it serves.
	SkillTools map[string][]string
	// ComposedRuntimeID, when set (execution.workers.composed), also registers the
	// ADR-0145 D12 composed-worker runtime under this id: the same runtime file,
	// launched `--composed --agent-id <id>`, holding no profile, no capability and
	// no grant of its own, and marked System so selection never picks it — the
	// dispatcher calls it with a composition bound to the step lease. "" ⇒ none.
	ComposedRuntimeID string
}

// IsProfileRuntime reports whether an agent record runs the profile runtime —
// either a profile agent, or the pre-part-B registration of worker_agent.py
// itself. The boot reconcile prunes such records when discovery no longer
// produced them (a profile deleted, renamed or now invalid), because their exec
// path still exists and the exec-path reconcile would keep them forever.
func IsProfileRuntime(rec AgentRecord) bool {
	return rec.Profile != "" || filepath.Base(filepath.FromSlash(rec.ExecPath)) == ProfileRuntimeFile
}

// ComputeProfileSourceHash is a profile agent's SourceHash: it covers the runtime
// AND the profile file, so editing either re-marks the agent provisional.
func ComputeProfileSourceHash(version string, runtime, profile []byte) string {
	r := sha256.Sum256(runtime)
	p := sha256.Sum256(profile)
	return ComputeSourceHash(version, []byte(hex.EncodeToString(r[:])+":"+hex.EncodeToString(p[:])))
}

// profileAgentRecords builds one DiscoveredAgent per valid profile under
// <agentsDir>/profiles. taken holds the ids the file scan already produced; a
// profile whose name collides with one is refused (two agents, one id).
func profileAgentRecords(agentsDir string, isSystemAgent func(string) bool, opt DiscoverOptions, taken map[string]bool) []DiscoveredAgent {
	dir := filepath.Join(agentsDir, ProfilesDirName)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil
	}
	loaded, err := agentprofile.LoadDir(dir, opt.SkillTools)
	if err != nil {
		slog.Error("agent profiles: profiles directory unreadable; no profile agents registered", "dir", dir, "err", err)
		return nil
	}
	if len(loaded) == 0 {
		return nil
	}

	runtime, rerr := os.ReadFile(filepath.Join(agentsDir, ProfileRuntimeFile))
	if rerr != nil {
		slog.Error("agent profiles: the profile runtime is missing; no profile agents registered",
			"runtime", filepath.Join(agentsDir, ProfileRuntimeFile), "profiles", len(loaded), "err", rerr)
		return nil
	}
	var out []DiscoveredAgent
	for _, l := range loaded {
		if l.Err != nil {
			slog.Error("agent profile INVALID: skipped, not registered as an agent (fix the file and restart)",
				"path", l.Path, "err", l.Err)
			continue
		}
		p := l.Profile
		for _, w := range p.Warnings {
			slog.Warn("agent profile warning", "profile", p.Name, "detail", w)
		}
		id := p.Name
		if taken[id] {
			slog.Error("agent profile INVALID: its name is already an agent id; skipped", "path", l.Path, "id", id)
			continue
		}
		taken[id] = true
		system := isSystemAgent(id)
		if p.TrustTier == "system" && !system {
			slog.Warn("agent profile declares trust_tier system but the composition root does not grant "+
				"system status; registered as a regular agent", "profile", id)
		}
		out = append(out, DiscoveredAgent{
			Agent: AgentRecord{
				ID:              id,
				Name:            humanizeProfileName(id),
				Description:     p.Description,
				Runtime:         "python",
				ExecPath:        ProfileRuntimeFile,
				Dir:             filepath.ToSlash(agentsDir),
				SourceHash:      ComputeProfileSourceHash(p.Version, runtime, l.Text),
				ManifestVersion: p.Version,
				Provisional:     true,
				System:          system,
				LaunchArgs:      []string{"--profile", p.Name, "--agent-id", id},
				Profile:         p.Name,
				TrustTier:       p.TrustTier,
				ModelPreference: p.Model,
			},
			// The manifest the runtime's manifest_json() describes
			// (cambrian_agent_sdk.profiles.Profile.manifest): capabilities route the
			// agent; the allowlist is under tool_allowlist, never Tools (still read
			// as legacy capability tags).
			Manifest: ManifestRecord{
				Version:          p.Version,
				Capabilities:     append([]string(nil), p.Capabilities...),
				SupportedFormats: []string{"text"},
				ToolAllowlist:    append([]string(nil), p.Tools...),
				Skills:           append([]string(nil), p.Skills...),
				Examples:         append([]string(nil), p.Examples...),
				StreamsOutput:    p.StreamsOutput,
				// ADR-0157 D1.2 (ENG-1960): the profile's declared paths reach the
				// manifest, so the boot admits them exactly like an AGENT_MANIFEST's
				// (agentmgr.AgentSandbox.admitDeclared against allowlist_cap).
				SandboxPaths: profileSandboxPaths(p.SandboxPaths),
				ReleaseNotes: "worker_agent profile " + p.Name + " (agent stack v2).",
			},
		})
	}
	if rt, ok := composedRuntimeRecord(agentsDir, opt.ComposedRuntimeID, runtime, taken, len(out)); ok {
		out = append(out, rt)
	}
	return out
}

// ComposedRuntimeVersion is the composed runtime's manifest version.
const ComposedRuntimeVersion = "1.0.0"

// composedRuntimeRecord builds the ADR-0145 D12 runtime agent: registered only when
// an id is configured, at least one valid profile exists (with none there is no
// profession to compose from), and the id is not already taken.
func composedRuntimeRecord(agentsDir, id string, runtime []byte, taken map[string]bool, profiles int) (DiscoveredAgent, bool) {
	if id == "" || profiles == 0 {
		return DiscoveredAgent{}, false
	}
	if taken[id] {
		slog.Error("composed worker runtime NOT registered: its id is already an agent id", "id", id)
		return DiscoveredAgent{}, false
	}
	taken[id] = true
	return DiscoveredAgent{
		Agent: AgentRecord{
			ID:              id,
			Name:            "Composed Worker",
			Description:     "Composed worker runtime (ADR-0145 D12): runs plan steps with the skills and tools the kernel composes for each step; never selected by capability.",
			Runtime:         "python",
			ExecPath:        ProfileRuntimeFile,
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      ComputeProfileSourceHash(ComposedRuntimeVersion, runtime, []byte("composed:"+id)),
			ManifestVersion: ComposedRuntimeVersion,
			Provisional:     true,
			// Kernel-invoked, never a selection candidate (domain.IsSystemDefinition):
			// granted here, by the kernel's own discovery of its own runtime file.
			System:     true,
			LaunchArgs: []string{"--composed", "--agent-id", id},
			TrustTier:  "first-party",
		},
		Manifest: ManifestRecord{
			Version:          ComposedRuntimeVersion,
			SupportedFormats: []string{"text"},
			ReleaseNotes:     "ADR-0145 D12 composed worker runtime.",
		},
	}, true
}

// profileSandboxPaths copies a profile's sandbox_paths into the manifest's
// record shape (nil when it declares none).
func profileSandboxPaths(in []domain.SandboxPath) []SandboxPathRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]SandboxPathRecord, len(in))
	for i, p := range in {
		out[i] = SandboxPathRecord{Path: p.Path, Write: p.Write}
	}
	return out
}

func humanizeProfileName(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
