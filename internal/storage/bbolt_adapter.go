package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"go.etcd.io/bbolt"

	"github.com/cambrian-sh/core/domain"
)

// sidecarManifest is the JSON shape of a *.manifest.json sidecar file.
type sidecarManifest struct {
	Trait            string   `json:"trait"`
	Version          string   `json:"version"`
	ExecPath         string   `json:"exec_path"`
	Description      string   `json:"description"`
	SupportedFormats []string `json:"supported_formats"`
	Runtime          string   `json:"runtime"`
	// ADR-0125: standalone sidecars are no longer tool-only, so they can carry
	// the manifest extras the Gatekeeper/SEC-01 read.
	Capabilities  []string `json:"capabilities"`
	Tools         []string `json:"tools"`
	MemoryLimitMB int      `json:"memory_limit_mb"`
}

// sidecarTraits is the explicit-trait vocabulary a standalone sidecar may
// declare (ADR-0125 D5). "cognitive" normalizes to the domain zero value "".
// A missing trait still skips: an unrelated *.manifest.json in the agents tree
// must not silently become a cognitive agent.
//
// "tool" is NOT accepted (owner decision 2026-11-14, ENG-1351): the trait is
// deleted, so a sidecar declaring it is skipped like any unknown trait.
var sidecarTraits = map[string]string{
	"cognitive": "",
	"model":     "model",
	"daemon":    "daemon",
}

var agentBucket = []byte("agents")
var manifestBucket = []byte("manifests")
var taskEventBucket = []byte("task_events")
var checkpointBucket = []byte("checkpoints")
var sessionBucket = []byte("sessions")
var runBucket = []byte("runs")

// artifactBucket is the RETIRED pre-ADR-0157 artifact metadata bucket. It is no
// longer created; a store that still has it is imported into the run ledger once
// (ADR-0157 Q6, kernel.importLegacyArtifacts) and the bucket dropped.
var artifactBucket = []byte("artifacts")
var watchConfigBucket = []byte("watch_configs")

// pipelineBucket holds authored reactive pipelines, keyed by pipeline id.
//
// The VALUE is an opaque JSON spec. Core stores it and never parses it: the
// pipeline schema is premium's (the graph model, the node vocabulary, the
// expression language), and a copy of that schema in OSS would be a second
// definition to keep in step. Core's job here is durability, not meaning.
var pipelineBucket = []byte("pipelines")
var planEventBucket = []byte("plan_events")
var retrievalSessionBucket = []byte("retrieval_sessions")
var traversalLogBucket = []byte("traversal_log")
var contradictionResolutionBucket = []byte("contradiction_resolutions")
var descRegex = regexp.MustCompile(`AGENT_DESCRIPTION\s*=\s*(?:"|')([^"']+)(?:"|')`)
var manifestRegex = regexp.MustCompile(`(?s)AGENT_MANIFEST\s*=\s*'''([\s\S]*?)'''`)

// manifestRegexJS is the JS twin of manifestRegex (ADR-0125): a module-level
// AGENT_MANIFEST assigned a backtick template literal. The sibling
// <id>.manifest.json remains the canonical form for JS agents; this exists for
// symmetry with the Python heredoc convenience.
var manifestRegexJS = regexp.MustCompile("(?s)AGENT_MANIFEST\\s*=\\s*`([\\s\\S]*?)`")

type BBoltAdapter struct {
	db *bbolt.DB
	// journalAppenders counts AppendReactiveSignal calls in flight, so a lone
	// appender commits directly instead of waiting out bbolt's batch window
	// (ADR-0061 A2.7; see AppendReactiveSignal).
	journalAppenders atomic.Int32
	CardFetcher      domain.CardFetcher // for A2A agent registration (agentcard.RemoteFetcher); nil means RegisterA2AAgent disabled
	Enqueuer         Enqueuer           // for profile-seeding enqueue after registration
}

func NewBBoltAdapter(dbPath string, agentsDir string, isSystemAgent func(id string) bool) (*BBoltAdapter, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("bbolt database open failed: %w", err)
	}

	adapter := &BBoltAdapter{db: db}

	// Create buckets and run initial seed
	if err := adapter.Seed(agentsDir, isSystemAgent); err != nil {
		db.Close()
		return nil, fmt.Errorf("bbolt seed failed: %w", err)
	}
	// ADR-0061 A2.10: move a pre-A2 flat journal into per-stream buckets.
	if _, _, err := migrateReactiveJournal(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateRetiredToolTrait(db); err != nil {
		db.Close()
		return nil, err
	}

	return adapter, nil
}

// NewBBoltAdapterNoScan opens the DB and ensures all buckets exist WITHOUT scanning any
// agents directory (ADR-0075). The composition root uses this when it registers the
// filesystem agents through an app-layer FilesystemAgentSource instead — so the built-in
// scan is just one AgentSource among many, not a hardcoded step inside the store.
func NewBBoltAdapterNoScan(dbPath string) (*BBoltAdapter, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("bbolt database open failed: %w", err)
	}
	adapter := &BBoltAdapter{db: db}
	if err := db.Update(createBuckets); err != nil {
		db.Close()
		return nil, fmt.Errorf("bbolt bucket init failed: %w", err)
	}
	// ADR-0061 A2.10: move a pre-A2 flat journal into per-stream buckets.
	if _, _, err := migrateReactiveJournal(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateRetiredToolTrait(db); err != nil {
		db.Close()
		return nil, err
	}
	return adapter, nil
}

// DiscoveredAgent bundles an agent record with its manifest, as produced by the
// filesystem discovery pass BEFORE persistence (ADR-0075). Separating discovery (pure
// file I/O) from persistence (the idempotent bbolt write) is the "invert Seed →
// discover-then-persist" refactor: the same discovery is reusable by an app-layer
// AgentSource so a plugin can contribute its OWN agents directory.
type DiscoveredAgent struct {
	Agent    AgentRecord
	Manifest ManifestRecord
}

// Seed creates buckets if they don't exist and populates the database from agentsDir.
// It now runs in two phases: DiscoverFilesystemAgents does all file I/O and record
// building outside the write transaction, then upsertDiscovered persists each record
// idempotently (existing agents update only when SourceHash changes). Five shapes are
// supported: single-file `*_agent.py`, a Python package (`__init__.py` + `agent.py`),
// single-file `*agent.ts`/`*agent.js`, a JS package (`package.json` + `agent.ts|js`)
// (ADR-0125), and standalone `*.manifest.json` sidecars. isSystemAgent (nil-safe)
// stamps AgentRecord.System.
//
// Separated from NewBBoltAdapter so callers that only need a DB handle (e.g. tests) can
// construct without a filesystem scan.
func (b *BBoltAdapter) Seed(agentsDir string, isSystemAgent func(id string) bool) error {
	// Phase 0: ensure all buckets exist.
	if err := b.db.Update(createBuckets); err != nil {
		return err
	}

	// Phase 1: discover (file I/O; no write tx held).
	discovered, walkErr := DiscoverFilesystemAgents(agentsDir, isSystemAgent)
	if walkErr != nil {
		slog.Warn("DB (BBOLT): agents dir unreadable, skipping seed", "dir", agentsDir, "err", walkErr)
	}

	// Phase 2: persist idempotently.
	if err := b.db.Update(func(tx *bbolt.Tx) error {
		for _, da := range discovered {
			if err := upsertDiscovered(tx, da); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return b.checkSystemAgentLayout(agentsDir)
}

// createBuckets creates every bbolt bucket the store uses if absent.
func createBuckets(tx *bbolt.Tx) error {
	for _, name := range [][]byte{
		agentBucket, manifestBucket, taskEventBucket, checkpointBucket, sessionBucket, runBucket,
		planEventBucket, retrievalSessionBucket,
		traversalLogBucket, contradictionResolutionBucket, watchConfigBucket, pipelineBucket,
		// REACT-01 (ADR-0061): durable reactive-execution buckets. The journal is
		// the A2 per-stream parent; the legacy flat bucket is never created, only
		// migrated (migrateReactiveJournal).
		reactiveJournalBucket, reactiveCursorBucket, reactiveIdempotencyBucket, reactiveDeadLetterBucket,
		// ADR-0090 D8.1: two-phase outbound delivery intents.
		deliveryIntentBucket,
	} {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	return nil
}

// DiscoverFilesystemAgents walks agentsDir and returns the agent records it finds,
// WITHOUT touching the database (ADR-0075). It reaches nested subdirectories such as
// agents/system/ (the privileged organs). A record whose source is unreadable or a
// sidecar whose trait/binary is invalid is skipped. isSystemAgent (nil-safe) stamps
// AgentRecord.System. This is the single discovery implementation used both by Seed
// (which then persists with manifests) and by the app-layer FilesystemAgentSource.
//
// Declarative profiles (<agentsDir>/profiles/*.yaml, profile_discovery.go) are
// discovered too, validated WITHOUT the skill checks; the composition root uses
// DiscoverFilesystemAgentsWith to pass the kernel's skill index.
func DiscoverFilesystemAgents(agentsDir string, isSystemAgent func(id string) bool) ([]DiscoveredAgent, error) {
	return DiscoverFilesystemAgentsWith(agentsDir, isSystemAgent, DiscoverOptions{})
}

// DiscoverFilesystemAgentsWith is DiscoverFilesystemAgents with options (the skill
// index profiles are validated against).
func DiscoverFilesystemAgentsWith(agentsDir string, isSystemAgent func(id string) bool, opt DiscoverOptions) ([]DiscoveredAgent, error) {
	systemID := isSystemAgent
	if systemID == nil {
		systemID = func(string) bool { return false }
	}
	var out []DiscoveredAgent
	profilesDir := filepath.Join(agentsDir, ProfilesDirName)
	runtimePath := filepath.Join(agentsDir, ProfileRuntimeFile)
	walkErr := filepath.WalkDir(agentsDir, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("DB (BBOLT): agents walk error, skipping", "path", walkPath, "err", walkErr)
			return nil
		}
		if d.IsDir() {
			if walkPath == agentsDir {
				return nil
			}
			// Profiles are data, read by profileAgentRecords below, never scanned
			// for agent source files.
			if walkPath == profilesDir {
				return fs.SkipDir
			}
			// ADR-0125: a dependency may legitimately ship a file named agent.js —
			// nothing under node_modules is an agent of ours.
			if d.Name() == "node_modules" {
				return fs.SkipDir
			}
			if isAgentPackage(walkPath) {
				id := d.Name()
				entry := filepath.Join(walkPath, "agent.py")
				if da, ok := pythonAgentRecord(agentsDir, entry, id, systemID); ok {
					out = append(out, da)
				}
				return fs.SkipDir
			}
			// JS package form (ADR-0125): package.json + agent.ts/agent.js.
			// Checked after the Python form, so Python wins when both are present.
			if entry := jsAgentPackageEntry(walkPath); entry != "" {
				if da, ok := jsAgentRecord(agentsDir, entry, d.Name(), systemID); ok {
					out = append(out, da)
				}
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		// The profile runtime is not an agent by itself: it runs once per profile,
		// under the profile's id (profile_discovery.go). Registering it too would
		// run the default profile a second time as "worker_agent".
		if walkPath == runtimePath {
			return nil
		}
		switch {
		case strings.HasSuffix(name, "agent.py"):
			id := strings.TrimSuffix(name, ".py")
			if da, ok := pythonAgentRecord(agentsDir, walkPath, id, systemID); ok {
				out = append(out, da)
			}
		case strings.HasSuffix(name, "agent.ts"), strings.HasSuffix(name, "agent.js"):
			id := strings.TrimSuffix(name, filepath.Ext(name))
			if da, ok := jsAgentRecord(agentsDir, walkPath, id, systemID); ok {
				out = append(out, da)
			}
		case strings.HasSuffix(name, ".manifest.json"):
			id := strings.TrimSuffix(name, ".manifest.json")
			// A manifest with a sibling source file is that record's manifest
			// (sidecar-preference inside pythonAgentRecord/jsAgentRecord), not a
			// standalone agent — processing it here would double-register the id.
			if hasSiblingAgentSource(walkPath, id) {
				return nil
			}
			if da, ok := sidecarAgentRecord(agentsDir, walkPath, id, systemID); ok {
				out = append(out, da)
			}
		}
		return nil
	})
	taken := make(map[string]bool, len(out))
	for _, da := range out {
		taken[da.Agent.ID] = true
	}
	out = append(out, profileAgentRecords(agentsDir, systemID, opt, taken)...)
	return out, walkErr
}

// UpsertDiscoveredAgent persists one DiscoveredAgent idempotently in its own transaction
// (ADR-0075). It is the exported, source-facing form of the built-in seeder's per-agent
// write: an unchanged agent (same SourceHash) is left untouched — crucially preserving
// its post-seeding Provisional=false state across reboots — so registering the built-in
// filesystem agents through an AgentSource behaves exactly like the old in-Seed scan.
// The one exception is System status, which is composition-root policy rather than a
// property of the agent's code and is therefore reconciled on every call.
func (b *BBoltAdapter) UpsertDiscoveredAgent(da DiscoveredAgent) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		return upsertDiscovered(tx, da)
	})
}

// upsertDiscovered persists one DiscoveredAgent idempotently: the manifest is always
// written; a brand-new agent is inserted Provisional; an existing agent is updated only
// when its SourceHash changed (then re-marked Provisional). Mirrors the pre-inversion
// per-agent seed semantics exactly.
func upsertDiscovered(tx *bbolt.Tx, da DiscoveredAgent) error {
	agentsBucket := tx.Bucket(agentBucket)
	manifestsBucket := tx.Bucket(manifestBucket)
	if agentsBucket == nil || manifestsBucket == nil {
		return fmt.Errorf("upsertDiscovered: required buckets missing")
	}
	id := da.Agent.ID
	manifestData, err := json.Marshal(da.Manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest for %s: %w", id, err)
	}
	if err := manifestsBucket.Put([]byte(id), manifestData); err != nil {
		return fmt.Errorf("put manifest for %s: %w", id, err)
	}

	existingData := agentsBucket.Get([]byte(id))
	if existingData == nil {
		slog.Info("DB (BBOLT): seeding new agent...", "id", id)
		data, err := json.Marshal(da.Agent)
		if err != nil {
			return fmt.Errorf("marshal new agent %s: %w", id, err)
		}
		return agentsBucket.Put([]byte(id), data)
	}

	var existing AgentRecord
	if json.Unmarshal(existingData, &existing) != nil {
		return nil // malformed existing record — leave as-is
	}
	// System status is reconciled BEFORE the source-hash short-circuit, because it
	// is a policy decision made by the composition root and not a property of the
	// agent's code.
	//
	// It used to be refreshed only on the "source changed" branch below, so
	// granting a plugin's agent system status had no effect until somebody
	// happened to edit that agent's source. The kernel went on LLM-interviewing (ADR-0037, since deleted)
	// and auctioning an organ the composition root had already declared
	// privileged, and the grant looked like it had simply not worked.
	//
	// A grant does NOT make the agent provisional: nothing about what it can do
	// changed, only how the kernel is allowed to route to it.
	if existing.System != da.Agent.System {
		slog.Info("DB (BBOLT): agent system status changed", "id", id, "system", da.Agent.System)
		existing.System = da.Agent.System
		data, err := json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("marshal updated agent %s: %w", id, err)
		}
		if err := agentsBucket.Put([]byte(id), data); err != nil {
			return fmt.Errorf("put agent %s: %w", id, err)
		}
	}
	if existing.SourceHash == da.Agent.SourceHash {
		return nil // unchanged
	}
	slog.Info("DB (BBOLT): agent source changed, marking Provisional", "id", id)
	// Preserve the record identity; refresh the source-derived fields + Provisional.
	existing.SourceHash = da.Agent.SourceHash
	existing.ManifestVersion = da.Agent.ManifestVersion
	existing.Provisional = true
	existing.Description = da.Agent.Description
	existing.System = da.Agent.System
	// Exec/runtime refresh on source change applies to EVERY record (ADR-0125).
	// It used to be gated on trait=="tool" — the only sidecar trait before
	// standalone sidecars opened up — which would have pinned a moved entry
	// point or a runtime override forever for the new shapes.
	existing.ExecPath = da.Agent.ExecPath
	existing.Runtime = da.Agent.Runtime
	// The trait is source-derived too: it used to stay as first registered, so an
	// organ moved off DeterministicAgent kept a stale "tool" record forever
	// (ENG-1351).
	existing.Trait = da.Agent.Trait
	// Agent stack v2 profile fields are source-derived too (the profile file is
	// part of the SourceHash), so they refresh with it.
	existing.LaunchArgs = da.Agent.LaunchArgs
	existing.Profile = da.Agent.Profile
	existing.TrustTier = da.Agent.TrustTier
	existing.ModelPreference = da.Agent.ModelPreference
	data, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("marshal updated agent %s: %w", id, err)
	}
	return agentsBucket.Put([]byte(id), data)
}

// isAgentPackage reports whether dir is a Python package that also ships the
// agent entry point. Detected by the presence of both `__init__.py` (the Python
// package marker) and `agent.py` (the seeder's convention for the gRPC entry).
func isAgentPackage(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "__init__.py")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.py")); err != nil {
		return false
	}
	return true
}

// jsAgentPackageEntry reports the entry file of a JS agent package (ADR-0125):
// a dir shipping package.json (the JS package marker) plus agent.ts or
// agent.js (the seeder's entry convention). Returns "" when dir is not one.
func jsAgentPackageEntry(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return ""
	}
	for _, entry := range []string{"agent.ts", "agent.js"} {
		if _, err := os.Stat(filepath.Join(dir, entry)); err == nil {
			return filepath.Join(dir, entry)
		}
	}
	return ""
}

// hasSiblingAgentSource reports whether a *.manifest.json has a sibling source
// file it belongs to (<id>.py / <id>.ts / <id>.js) — then it is that record's
// manifest, not a standalone sidecar agent.
func hasSiblingAgentSource(manifestPath, id string) bool {
	dir := filepath.Dir(manifestPath)
	for _, ext := range []string{".py", ".ts", ".js"} {
		if _, err := os.Stat(filepath.Join(dir, id+ext)); err == nil {
			return true
		}
	}
	return false
}

// jsAgentRecord builds the DiscoveredAgent for an `*agent.ts` / `*agent.js`
// entry (ADR-0125), mirroring pythonAgentRecord: a sibling <id>.manifest.json
// is canonical; an embedded AGENT_MANIFEST = `{…}` template literal is the
// fallback; AGENT_DESCRIPTION parses with the shared regex (it is anchor-free).
// Runtime: the manifest's runtime field wins; else .ts → bun, .js → node.
func jsAgentRecord(agentsDir, fullPath, id string, isSystemAgent func(string) bool) (DiscoveredAgent, bool) {
	content, readErr := os.ReadFile(fullPath)

	description := "General-purpose agent."
	if readErr == nil {
		if m := descRegex.FindSubmatch(content); len(m) > 1 {
			description = string(m[1])
		}
	}

	var manifest ManifestRecord
	sidecarUsed := false
	sibling := filepath.Join(filepath.Dir(fullPath), id+".manifest.json")
	if mb, e := os.ReadFile(sibling); e == nil {
		var sm ManifestRecord
		if json.Unmarshal(mb, &sm) == nil && sm.Trait != "tool" {
			manifest = sm
			sidecarUsed = true
		} else {
			slog.Warn("DB (BBOLT): sibling manifest ignored (parse error or trait=tool)", "id", id, "file", sibling)
		}
	}
	if !sidecarUsed && readErr == nil {
		if m := manifestRegexJS.FindSubmatch(content); len(m) > 1 {
			if jsonErr := json.Unmarshal(m[1], &manifest); jsonErr != nil {
				slog.Warn("DB (BBOLT): agent manifest JSON parse error", "id", id, "err", jsonErr)
				manifest = ManifestRecord{}
			}
		}
	}

	runtime := "bun"
	if strings.HasSuffix(fullPath, ".js") {
		runtime = "node"
	}
	if manifest.Runtime != "" {
		runtime = manifest.Runtime
	}

	var fileContent []byte
	if readErr == nil {
		fileContent = content
	}
	sourceHash := ComputeSourceHash(manifest.Version, fileContent)

	// ENG-1351: the "tool" trait is deleted; a manifest still declaring it is
	// refused (the agent is not registered) rather than silently re-typed.
	if manifest.Trait == "tool" {
		slog.Warn("DB (BBOLT): agent manifest declares the retired trait \"tool\"; not registered (subclass CognitiveAgent, or expose the capability as a kernel tool)", "id", id, "file", fullPath)
		return DiscoveredAgent{}, false
	}
	execPath := filepath.ToSlash(fullPath)
	// ExecPath must be relative to Dir — buildAgentCmd resolves it under
	// cmd.Dir=def.Dir (same rule as pythonAgentRecord).
	if rel, relErr := filepath.Rel(agentsDir, fullPath); relErr == nil && !strings.HasPrefix(rel, "..") {
		execPath = filepath.ToSlash(rel)
	}

	return DiscoveredAgent{
		Agent: AgentRecord{
			ID:              id,
			Name:            agentDisplayName(id),
			Description:     description,
			Runtime:         runtime,
			ExecPath:        execPath,
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      sourceHash,
			ManifestVersion: manifest.Version,
			Provisional:     true,
			Trait:           manifest.Trait,
			System:          isSystemAgent(id),
		},
		Manifest: manifest,
	}, true
}

// checkSystemAgentLayout warns when a System=true agent lives outside the
// <agentsDir>/system/ subtree. The file is honored; the warning is a soft
// signal that the package-layout convention is broken.
func (b *BBoltAdapter) checkSystemAgentLayout(agentsDir string) error {
	systemDir := filepath.ToSlash(filepath.Join(agentsDir, "system"))
	// ExecPath is RELATIVE to Dir (see discoverAgent: an absolute path produces a
	// doubled "agents/agents/..." and python cannot open the file), so the prefix
	// this compares against must be relative too.
	//
	// It used to be the ABSOLUTE systemDir, which no relative ExecPath can ever
	// match — so the warning fired for every correctly-placed system agent and the
	// deployment log said the convention was broken four times a boot when it was
	// not. A check that cannot pass is worse than no check: it teaches people to
	// ignore the line that would matter when something really is misplaced.
	const systemDirPrefix = "system/"
	return b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec AgentRecord
			if json.Unmarshal(v, &rec) != nil {
				return nil
			}
			if rec.System && !strings.HasPrefix(rec.ExecPath, systemDirPrefix) {
				slog.Warn("DB (BBOLT): system agent registered outside agents/system/ subtree — file honored, convention broken",
					"id", rec.ID, "exec_path", rec.ExecPath, "system_dir", systemDir)
			}
			return nil
		})
	})
}

// pythonAgentRecord builds the DiscoveredAgent for a `*_agent.py` entry (no DB access).
// Manifest resolution prefers a sibling `<id>.manifest.json` (a non-"tool" ManifestRecord)
// over the embedded `AGENT_MANIFEST=”'…”'` regex (ADR-0075 sidecar-preference) — the
// declared JSON is less brittle than parsing a manifest out of source text. ok is always
// true (an unreadable entry still registers with defaults, preserving pre-inversion
// behavior; availability health-gating is applied by the app-layer source, not here).
func pythonAgentRecord(agentsDir, fullPath, id string, isSystemAgent func(string) bool) (DiscoveredAgent, bool) {
	content, readErr := os.ReadFile(fullPath)

	description := "General-purpose agent."
	if readErr == nil {
		if m := descRegex.FindSubmatch(content); len(m) > 1 {
			description = string(m[1])
		}
	}

	var manifest ManifestRecord
	sidecarUsed := false
	// Prefer a sibling declared manifest (non-tool) over the source regex.
	sibling := filepath.Join(filepath.Dir(fullPath), id+".manifest.json")
	if mb, e := os.ReadFile(sibling); e == nil {
		var sm ManifestRecord
		if json.Unmarshal(mb, &sm) == nil && sm.Trait != "tool" {
			manifest = sm
			sidecarUsed = true
		} else {
			slog.Warn("DB (BBOLT): sibling manifest ignored (parse error or trait=tool)", "id", id, "file", sibling)
		}
	}
	if !sidecarUsed && readErr == nil {
		if m := manifestRegex.FindSubmatch(content); len(m) > 1 {
			if jsonErr := json.Unmarshal(m[1], &manifest); jsonErr != nil {
				slog.Warn("DB (BBOLT): agent manifest JSON parse error", "id", id, "err", jsonErr)
				manifest = ManifestRecord{}
			}
		}
	}

	var fileContent []byte
	if readErr == nil {
		fileContent = content
	}
	sourceHash := ComputeSourceHash(manifest.Version, fileContent)

	// ENG-1351: the "tool" trait is deleted; a manifest still declaring it is
	// refused (the agent is not registered) rather than silently re-typed.
	if manifest.Trait == "tool" {
		slog.Warn("DB (BBOLT): agent manifest declares the retired trait \"tool\"; not registered (subclass CognitiveAgent, or expose the capability as a kernel tool)", "id", id, "file", fullPath)
		return DiscoveredAgent{}, false
	}
	execPath := filepath.ToSlash(fullPath)
	// ExecPath must be relative to Dir — buildAgentCmd resolves it under cmd.Dir=def.Dir,
	// so a full path produces a doubled 'agents/agents/...' and python can't open the file.
	if rel, relErr := filepath.Rel(agentsDir, fullPath); relErr == nil && !strings.HasPrefix(rel, "..") {
		execPath = filepath.ToSlash(rel)
	}

	return DiscoveredAgent{
		Agent: AgentRecord{
			ID:              id,
			Name:            agentDisplayName(id),
			Description:     description,
			Runtime:         "python",
			ExecPath:        execPath,
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      sourceHash,
			ManifestVersion: manifest.Version,
			Provisional:     true,
			Trait:           manifest.Trait,
			System:          isSystemAgent(id),
		},
		Manifest: manifest,
	}, true
}

// sidecarAgentRecord builds the DiscoveredAgent for a standalone `*.manifest.json`
// sidecar (no DB access). Returns ok=false (skips) when the manifest is unreadable/
// unparseable, its trait is missing or unknown (ADR-0125: tool/cognitive/model/daemon
// are accepted; cognitive normalizes to ""), or its exec target does not exist — a
// health gate at discovery time.
func sidecarAgentRecord(agentsDir, manifestPath, id string, isSystemAgent func(string) bool) (DiscoveredAgent, bool) {
	manifestBytes, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		slog.Warn("DB (BBOLT): sidecar manifest unreadable, skipping", "file", manifestPath, "err", readErr)
		return DiscoveredAgent{}, false
	}
	var sm sidecarManifest
	if jsonErr := json.Unmarshal(manifestBytes, &sm); jsonErr != nil {
		slog.Warn("DB (BBOLT): sidecar manifest JSON parse error, skipping", "file", manifestPath, "err", jsonErr)
		return DiscoveredAgent{}, false
	}
	trait, known := sidecarTraits[sm.Trait]
	if !known {
		slog.Warn("DB (BBOLT): sidecar manifest trait missing or unknown, skipping", "file", manifestPath, "trait", sm.Trait)
		return DiscoveredAgent{}, false
	}
	manifestDir := filepath.Dir(manifestPath)
	resolvedExecPath := filepath.ToSlash(filepath.Join(manifestDir, filepath.FromSlash(sm.ExecPath)))
	binaryBytes, binErr := os.ReadFile(resolvedExecPath)
	if binErr != nil {
		slog.Warn("DB (BBOLT): sidecar binary not found, skipping", "exec_path", resolvedExecPath, "err", binErr)
		return DiscoveredAgent{}, false
	}
	runtime := "binary"
	if sm.Runtime != "" {
		runtime = sm.Runtime
	}
	agentsDirSlash := strings.TrimRight(filepath.ToSlash(agentsDir), "/") + "/"
	return DiscoveredAgent{
		Agent: AgentRecord{
			ID:              id,
			Name:            id,
			Description:     sm.Description,
			Runtime:         runtime,
			ExecPath:        strings.TrimPrefix(resolvedExecPath, agentsDirSlash),
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      ComputeSidecarSourceHash(sm.Version, manifestBytes, binaryBytes),
			ManifestVersion: sm.Version,
			Provisional:     true,
			Trait:           trait,
			System:          isSystemAgent(id),
		},
		Manifest: ManifestRecord{
			Version:          sm.Version,
			SupportedFormats: sm.SupportedFormats,
			Trait:            trait,
			Runtime:          sm.Runtime,
			Capabilities:     sm.Capabilities,
			Tools:            sm.Tools,
			MemoryLimitMB:    sm.MemoryLimitMB,
		},
	}, true
}

// GetAgentRecord returns the raw AgentRecord for the given name or ID.
func (b *BBoltAdapter) GetAgentRecord(name string) (*AgentRecord, error) {
	var rec AgentRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(name))
		if data != nil {
			if err := json.Unmarshal(data, &rec); err == nil {
				found = true
				return nil
			}
		}
		return bucket.ForEach(func(k, v []byte) error {
			var a AgentRecord
			if json.Unmarshal(v, &a) == nil {
				if a.Name == name {
					rec = a
					found = true
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("agent not found in database: %s", name)
	}
	return &rec, nil
}

// GetAllAgentRecords returns raw AgentRecords for every agent.
func (b *BBoltAdapter) GetAllAgentRecords() ([]AgentRecord, error) {
	var recs []AgentRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		return bucket.ForEach(func(_, v []byte) error {
			var a AgentRecord
			if err := json.Unmarshal(v, &a); err == nil {
				recs = append(recs, a)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// WriteManifestRecord persists a ManifestRecord for the given agent ID (ADR-0075). It is
// the manifest half of SetAgentWithManifest, so an AgentSource can carry the manifest
// EXTRAS (PythonDeps/MemoryLimitMB/schemas) the plain WriteAgentRecord path does not.
func (b *BBoltAdapter) WriteManifestRecord(agentID string, rec ManifestRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("manifest marshal for %s: %w", agentID, err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(manifestBucket)
		if bucket == nil {
			return fmt.Errorf("manifests bucket not found")
		}
		return bucket.Put([]byte(agentID), data)
	})
}

// GetManifestRecord retrieves the persisted ManifestRecord for the given agent ID.
func (b *BBoltAdapter) GetManifestRecord(agentID string) (*ManifestRecord, error) {
	var rec ManifestRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(manifestBucket)
		if mb == nil {
			return nil
		}
		data := mb.Get([]byte(agentID))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, fmt.Errorf("manifest read failed: %w", err)
	}
	return &rec, nil
}

// GetManifestRecordBatch returns ManifestRecords for all given IDs in a single read Tx.
func (b *BBoltAdapter) GetManifestRecordBatch(ids []string) (map[string]ManifestRecord, error) {
	result := make(map[string]ManifestRecord, len(ids))
	err := b.db.View(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(manifestBucket)
		if mb == nil {
			return nil
		}
		for _, id := range ids {
			data := mb.Get([]byte(id))
			if data == nil {
				continue
			}
			var rec ManifestRecord
			if json.Unmarshal(data, &rec) == nil {
				result[id] = rec
			}
		}
		return nil
	})
	return result, err
}

// WriteTaskEventRecord persists a TaskEventRecord to the task_events bucket.
func (b *BBoltAdapter) WriteTaskEventRecord(event TaskEventRecord) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("task_event marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return fmt.Errorf("task_events bucket not found")
		}
		return bucket.Put([]byte(event.TaskID), data)
	})
}

// ReadTaskEventRecord returns the TaskEventRecord stored under taskID, or nil.
func (b *BBoltAdapter) ReadTaskEventRecord(taskID string) (*TaskEventRecord, error) {
	var rec TaskEventRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(taskID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

// ReadTaskEventRecords returns all TaskEventRecords for the given (agentID, sourceHash) pair.
func (b *BBoltAdapter) ReadTaskEventRecords(agentID, sourceHash string) ([]TaskEventRecord, error) {
	var recs []TaskEventRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if err := json.Unmarshal(v, &e); err == nil {
				if e.AgentID == agentID && e.SourceHash == sourceHash {
					recs = append(recs, e)
				}
			}
			return nil
		})
	})
	return recs, err
}

// ReadAllTaskEventRecords returns every TaskEventRecord in the task_events bucket.
// Backs the ROUTE-05 bid-calibration extraction (agent, bid_confidence, verifier_score).
func (b *BBoltAdapter) ReadAllTaskEventRecords() ([]TaskEventRecord, error) {
	var recs []TaskEventRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if json.Unmarshal(v, &e) == nil {
				recs = append(recs, e)
			}
			return nil
		})
	})
	return recs, err
}

// ReadAllAgentIDs returns distinct "agentID:sourceHash" strings from the
// task_events bucket.
func (b *BBoltAdapter) ReadAllAgentIDs() ([]string, error) {
	seen := make(map[string]bool)
	var keys []string

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if err := json.Unmarshal(v, &e); err == nil && e.AgentID != "" {
				key := e.AgentID + ":" + e.SourceHash
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
			return nil
		})
	})
	return keys, err
}

// SetProvisional updates the Provisional flag for the given agent.
func (b *BBoltAdapter) SetProvisional(agentID string, provisional bool) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(agentID))
		if data == nil {
			return fmt.Errorf("agent %s not found", agentID)
		}
		var rec AgentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Provisional = provisional
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(agentID), updated)
	})
}

// SetCapabilities updates the Capabilities slice for the given agent in the agents bucket.
// Returns an error if the agent does not exist.
func (b *BBoltAdapter) SetCapabilities(agentID string, caps []string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(agentID))
		if data == nil {
			return fmt.Errorf("agent %s not found", agentID)
		}
		var rec AgentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Capabilities = caps
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(agentID), updated)
	})
}

// checkpointKey renders the (run, step) key.
//
// The step index is ZERO-PADDED because bbolt cursors are lexicographic: with an unpadded
// decimal, step "10" sorts before step "2", so "the latest checkpoint" was whichever step
// happened to sort last. Nine digits is far more steps than any plan will ever have and
// keeps the key fixed-width.
func checkpointKey(runID domain.RunID, stepIndex int) []byte {
	return []byte(fmt.Sprintf("%s:%09d", runID, stepIndex))
}

type checkpointRecord struct {
	RunID     domain.RunID      `json:"run_id"`
	SessionID domain.SessionID  `json:"session_id"`
	PlanID    string            `json:"plan_id"`
	StepIndex int               `json:"step_index"`
	Context   map[string]string `json:"context"`
	Timestamp time.Time         `json:"timestamp"`
}

// SaveCheckpoint persists a context checkpoint under its RUN.
func (b *BBoltAdapter) SaveCheckpoint(_ context.Context, runID domain.RunID, stepIndex int, ctx map[string]string) error {
	rec := checkpointRecord{
		RunID:     runID,
		StepIndex: stepIndex,
		Context:   ctx,
		Timestamp: time.Now(),
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put(checkpointKey(runID, stepIndex), data)
	})
}

// LoadCheckpoint loads one checkpoint of a run.
func (b *BBoltAdapter) LoadCheckpoint(_ context.Context, runID domain.RunID, stepIndex int) (map[string]string, error) {
	var ctx map[string]string
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get(checkpointKey(runID, stepIndex))
		if data == nil {
			return nil
		}
		var rec checkpointRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		ctx = rec.Context
		return nil
	})
	return ctx, err
}

// ListCheckpoints returns a run's checkpoints in ascending step order. The zero-padded key
// makes the cursor's lexicographic order the numeric order, so no post-sort is needed.
func (b *BBoltAdapter) ListCheckpoints(_ context.Context, runID domain.RunID) ([]domain.CheckpointMeta, error) {
	prefix := []byte(string(runID) + ":")
	var out []domain.CheckpointMeta
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var rec checkpointRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			out = append(out, domain.CheckpointMeta{
				RunID:     rec.RunID,
				SessionID: rec.SessionID,
				PlanID:    rec.PlanID,
				StepIndex: rec.StepIndex,
				Timestamp: rec.Timestamp,
			})
		}
		return nil
	})
	return out, err
}

// Compile-time proof that this adapter satisfies the checkpoint and run ports.
//
// These exist because the wiring is resolved by a RUNTIME type assertion
// (Server.executorCheckpointStore). When the interface changed and this implementation did
// not, the assertion simply stopped matching: checkpoints silently stopped being written,
// with no build error, no runtime error, and no failing test. A compile-time assertion turns
// that class of drift back into a build failure.
var (
	_ domain.CheckpointStore = (*BBoltAdapter)(nil)
	_ domain.RunStore        = (*BBoltAdapter)(nil)
)

// ── domain.RunStore ──────────────────────────────────────────────────────────

// SaveRun persists a run (including its plan, so a resume replays the same steps).
func (b *BBoltAdapter) SaveRun(_ context.Context, run domain.Run) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(runBucket)
		data, err := json.Marshal(run)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(run.ID), data)
	})
}

// GetRun reads a run by id; (nil, nil) when unknown.
func (b *BBoltAdapter) GetRun(_ context.Context, runID domain.RunID) (*domain.Run, error) {
	var run *domain.Run
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(runBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(runID))
		if data == nil {
			return nil
		}
		var r domain.Run
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		run = &r
		return nil
	})
	return run, err
}

// ListRunsForSession returns every run of a session, oldest first.
func (b *BBoltAdapter) ListRunsForSession(_ context.Context, sessionID domain.SessionID) ([]domain.Run, error) {
	var out []domain.Run
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(runBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var r domain.Run
			if json.Unmarshal(v, &r) != nil {
				return nil
			}
			if r.SessionID == sessionID {
				out = append(out, r)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// WriteAgentRecord persists an AgentRecord to the agents bucket.
func (b *BBoltAdapter) WriteAgentRecord(rec AgentRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("agent marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// DeleteAgentRecord removes an agent from the agents bucket and its companion
// entry from the manifests bucket, in a single transaction. It is the storage
// primitive behind the startup registry reconcile: a model dropped from config
// or an agent whose source file was deleted must not linger as an auction
// candidate (GetAllAgents reads this bucket). Idempotent by design — a missing
// id is not an error, so reconcile can delete a computed orphan set without a
// prior existence check.
func (b *BBoltAdapter) DeleteAgentRecord(id string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		// Best-effort manifest cleanup: the manifest bucket may be absent in
		// minimal stores (e.g. model-only registries), which is not an error.
		if mb := tx.Bucket(manifestBucket); mb != nil {
			return mb.Delete([]byte(id))
		}
		return nil
	})
}

// ListLegacyArtifactRecords returns every record of the retired artifacts bucket
// (ADR-0157 Q6), nil when the bucket does not exist. A malformed record fails the
// listing: the bucket must not be dropped with an unimported record in it.
func (b *BBoltAdapter) ListLegacyArtifactRecords() ([]ArtifactRecord, error) {
	var recs []ArtifactRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(artifactBucket)
		if bucket == nil {
			return nil
		}
		recs = []ArtifactRecord{}
		return bucket.ForEach(func(k, v []byte) error {
			var rec ArtifactRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("legacy artifact record %q: %w", k, err)
			}
			recs = append(recs, rec)
			return nil
		})
	})
	return recs, err
}

// DropLegacyArtifactBucket deletes the retired artifacts bucket (ADR-0157 Q6),
// after its records were imported into the run ledger. Absent is not an error.
func (b *BBoltAdapter) DropLegacyArtifactBucket() error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(artifactBucket) == nil {
			return nil
		}
		return tx.DeleteBucket(artifactBucket)
	})
}

// SaveSessionRecord persists a SessionRecord to the sessions bucket.
func (b *BBoltAdapter) SaveSessionRecord(rec SessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return fmt.Errorf("sessions bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// GetSessionRecord reads a SessionRecord by ID from the sessions bucket.
func (b *BBoltAdapter) GetSessionRecord(id string) (*SessionRecord, error) {
	var rec *SessionRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return nil
		}
		rec = &SessionRecord{}
		return json.Unmarshal(data, rec)
	})
	return rec, err
}

// ListSessionRecords returns all sessions, optionally filtered by status.
func (b *BBoltAdapter) ListSessionRecords(status string) ([]SessionRecord, error) {
	var recs []SessionRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var rec SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip malformed records
			}
			if status == "" || rec.Status == status {
				recs = append(recs, rec)
			}
			return nil
		})
	})
	return recs, err
}

// ── PlanEvent ────────────────────────────────────────────────────────────────

func (b *BBoltAdapter) WritePlanEventRecord(event PlanEventRecord) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("plan_event marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(planEventBucket)
		if bucket == nil {
			return fmt.Errorf("plan_events bucket not found")
		}
		return bucket.Put([]byte(event.PlanID), data)
	})
}

func (b *BBoltAdapter) ReadPlanEventRecord(planID string) (*PlanEventRecord, error) {
	var rec PlanEventRecord
	found := false
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(planEventBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(planID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

// ── RetrievalSession ─────────────────────────────────────────────────────────

func (b *BBoltAdapter) WriteRetrievalSessionRecord(session RetrievalSessionRecord) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("retrieval_session marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return fmt.Errorf("retrieval_sessions bucket not found")
		}
		return bucket.Put([]byte(session.SessionID), data)
	})
}

func (b *BBoltAdapter) ReadRetrievalSessionRecord(sessionID string) (*RetrievalSessionRecord, error) {
	var rec RetrievalSessionRecord
	found := false
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(sessionID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

func (b *BBoltAdapter) UpdateRetrievalSessionPlanOutcome(sessionID, planID, outcome string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return fmt.Errorf("retrieval_sessions bucket not found")
		}
		data := bucket.Get([]byte(sessionID))
		if data == nil {
			return nil
		}
		var rec RetrievalSessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.PlanID = planID
		rec.PlanOutcome = outcome
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(sessionID), updated)
	})
}

// ── TraversalLogEntry ────────────────────────────────────────────────────────

func (b *BBoltAdapter) WriteTraversalLogEntryRecord(entry TraversalLogEntryRecord) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("traversal_log_entry marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(traversalLogBucket)
		if bucket == nil {
			return fmt.Errorf("traversal_log bucket not found")
		}
		return bucket.Put([]byte(entry.EntryID), data)
	})
}

func (b *BBoltAdapter) UpdateTraversalLogPlanOutcome(entryID, planID, outcome string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(traversalLogBucket)
		if bucket == nil {
			return fmt.Errorf("traversal_log bucket not found")
		}
		data := bucket.Get([]byte(entryID))
		if data == nil {
			return nil
		}
		var rec TraversalLogEntryRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.PlanID = planID
		rec.PlanOutcome = outcome
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(entryID), updated)
	})
}

// ── ContradictionResolution ──────────────────────────────────────────────────

func (b *BBoltAdapter) WriteContradictionResolutionRecord(res ContradictionResolutionRecord) error {
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("contradiction_resolution marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(contradictionResolutionBucket)
		if bucket == nil {
			return fmt.Errorf("contradiction_resolutions bucket not found")
		}
		return bucket.Put([]byte(res.ResolutionID), data)
	})
}

// To clear the database lock on system shutdown
func (b *BBoltAdapter) Close() error {
	return b.db.Close()
}

// NewStepCache creates a BBoltStepCache backed by the same database file.
// The step_cache bucket is created if it does not already exist.
func (b *BBoltAdapter) NewStepCache() (*BBoltStepCache, error) {
	return NewBBoltStepCache(b.db)
}

// ── WatchConfig ───────────────────────────────────────────────────────────────

// WriteWatchConfig persists a WatchConfigRecord keyed by its ID. ADR-0032.
func (b *BBoltAdapter) WriteWatchConfig(rec WatchConfigRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("watch_config marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// ReadWatchConfig returns the WatchConfigRecord stored under id, or an error if absent.
func (b *BBoltAdapter) ReadWatchConfig(id string) (*WatchConfigRecord, error) {
	var rec WatchConfigRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("watch_config %q not found", id)
	}
	return &rec, nil
}

// ReadAllWatchConfigs returns all WatchConfigRecords from the bucket.
func (b *BBoltAdapter) ReadAllWatchConfigs() ([]WatchConfigRecord, error) {
	var recs []WatchConfigRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var rec WatchConfigRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			recs = append(recs, rec)
			return nil
		})
	})
	return recs, err
}

// DeleteWatchConfig removes the WatchConfigRecord with the given id.
// Returns an error if the id does not exist.
func (b *BBoltAdapter) DeleteWatchConfig(id string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		if bucket.Get([]byte(id)) == nil {
			return fmt.Errorf("watch_config %q not found", id)
		}
		return bucket.Delete([]byte(id))
	})
}

// SetWatchConfigActive updates only the Active field of an existing WatchConfigRecord.
func (b *BBoltAdapter) SetWatchConfigActive(id string, active bool) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("watch_config %q not found", id)
		}
		var rec WatchConfigRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Active = active
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(id), updated)
	})
}

// ── Reactive pipelines ────────────────────────────────────────────────────────

// WritePipeline persists one pipeline spec under its id.
//
// The spec is opaque bytes by design — see pipelineBucket. Rewriting the whole
// spec is the only update: a pipeline's lifecycle state lives INSIDE it, so a
// separate "set active" would be a second place that could disagree with the
// graph about whether it is live.
func (b *BBoltAdapter) WritePipeline(id string, spec []byte) error {
	if id == "" {
		return fmt.Errorf("pipeline id is required")
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(pipelineBucket)
		if bkt == nil {
			return fmt.Errorf("pipelines bucket not found")
		}
		return bkt.Put([]byte(id), spec)
	})
}

// ReadAllPipelines returns every stored pipeline spec, keyed by id.
func (b *BBoltAdapter) ReadAllPipelines() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := b.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(pipelineBucket)
		if bkt == nil {
			return nil // no bucket yet is "none stored", not a failure
		}
		return bkt.ForEach(func(k, v []byte) error {
			// bbolt's bytes are only valid for the life of the transaction.
			cp := make([]byte, len(v))
			copy(cp, v)
			out[string(k)] = cp
			return nil
		})
	})
	return out, err
}

// DeletePipeline removes one pipeline spec.
func (b *BBoltAdapter) DeletePipeline(id string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(pipelineBucket)
		if bkt == nil {
			return fmt.Errorf("pipelines bucket not found")
		}
		return bkt.Delete([]byte(id))
	})
}

// agentDisplayName turns a discovered agent id into its display name:
// "kg_extractor_agent" → "Kg Extractor Agent". It reproduces strings.Title exactly
// (deprecated, SA1019) over the id with "_" read as a space, so every stored name is
// unchanged: a letter is upper-cased when the rune before it is a separator in
// strings.Title's sense (TestAgentDisplayName_MatchesStringsTitle pins the
// equivalence). A previous `strings.ReplaceAll(…, " Agent", " Agent")` around it
// replaced a string with itself and was dropped.
func agentDisplayName(id string) string {
	prev := ' '
	return strings.Map(func(r rune) rune {
		out := r
		if titleSeparator(prev) {
			out = unicode.ToTitle(r)
		}
		prev = r
		return out
	}, strings.ReplaceAll(id, "_", " "))
}

// titleSeparator is strings.Title's word-boundary rule (strings.isSeparator).
func titleSeparator(r rune) bool {
	if r <= 0x7F {
		switch {
		case '0' <= r && r <= '9', 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', r == '_':
			return false
		}
		return true
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	return unicode.IsSpace(r)
}

// migrateRetiredToolTrait rewrites registry records stored with the deleted
// "tool" trait (owner decision 2026-11-14, ENG-1351). Discovery never refreshed a
// record's trait before, so a kernel database written before the 2026-10-31
// organ migration can still hold "tool" for a system organ (docling,
// kg_extractor, reranker). Such a record must never surface as an ordinary
// cognitive agent — the verifier pool and the profile seeder excluded "tool":
//   - a SYSTEM record becomes cognitive: system organs stay excluded from the
//     verifier pool and selection by IsSystemDefinition, whatever their trait;
//   - any other record is DELETED from the registry with a WARN: a third-party
//     tool agent is refused, exactly as discovery now refuses its manifest.
//
// Idempotent; runs at every open.
func migrateRetiredToolTrait(db *bbolt.DB) error {
	return db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(agentBucket)
		if bkt == nil {
			return nil
		}
		type change struct {
			key  []byte
			data []byte // nil = delete
		}
		var changes []change
		if err := bkt.ForEach(func(k, v []byte) error {
			var rec AgentRecord
			if json.Unmarshal(v, &rec) != nil || rec.Trait != "tool" {
				return nil
			}
			key := append([]byte(nil), k...)
			if rec.System {
				rec.Trait = ""
				data, err := json.Marshal(rec)
				if err != nil {
					return fmt.Errorf("re-type stored agent %s: %w", rec.ID, err)
				}
				slog.Info("DB (BBOLT): stored system agent re-typed from the retired trait \"tool\" to cognitive", "id", rec.ID)
				changes = append(changes, change{key, data})
				return nil
			}
			slog.Warn("DB (BBOLT): stored agent with the retired trait \"tool\" removed from the registry", "id", rec.ID)
			changes = append(changes, change{key, nil})
			return nil
		}); err != nil {
			return err
		}
		for _, c := range changes {
			var err error
			if c.data == nil {
				err = bkt.Delete(c.key)
			} else {
				err = bkt.Put(c.key, c.data)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}
