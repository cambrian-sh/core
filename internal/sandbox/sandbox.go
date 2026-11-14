// Package sandbox contains a process the kernel launches according to the
// TRUST TIER of the code it runs (ADR-0139 D8, owner Q1: "a tiered sandbox,
// third-party agents untrusted"; decisions/agent-stack-v2-mcp-tools-and-profiles.md:
// "stdio servers get the env allowlist and are sandboxed by trust tier").
//
// The package decides three things, in this order:
//
//  1. WHICH TIER a server runs at (Policy.Resolve): the server's own declared
//     trust_tier, else its integration-catalog entry's tier mapped through
//     catalog_tiers, else first-party for a plugin-contributed server, else the
//     configured default tier (third-party: unknown code is untrusted).
//  2. WHAT CONTAINMENT that tier wants and what this platform can deliver
//     (plan): every tier wants the env allowlist, a dedicated working directory,
//     memory/process/CPU-time limits, a stall kill, a per-message output cap and
//     a killable process tree; third-party additionally wants its filesystem
//     writes confined, network egress off unless declared, and a low-privilege
//     identity. A wanted control the platform cannot enforce is MISSING, and the
//     tier's on_unavailable decides: "refuse" (fail closed; the third-party
//     default) or "degrade" (start, and report why).
//  3. HOW to launch it (Start): the native runner (Windows Job Object +
//     low-integrity token; Linux/Unix sh ulimit wrapper, bubblewrap when present,
//     an optional cgroup v2 parent) or the Docker runner.
//
// The environment allowlist itself is internal/procenv's; this package never
// widens it. The Report it returns is what the operator console shows as the
// server's effective containment.
//
// Imports: the standard library and golang.org/x/sys only, so any launcher can
// use it (the MCP connector today; kernel-spawned agents are the planned second
// caller, T-20261106-kernel-spawned-agents-are-not-contained-by).
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tier is a containment tier. "system" (the kernel itself) is never a tier a
// launched process can hold.
type Tier string

// The launch tiers, weakest containment first.
const (
	// TierFirstParty is code Cambrian ships itself (a premium plugin's server,
	// a first-party agent profile, cambrian-core's tools/code_sandbox).
	TierFirstParty Tier = "first-party"
	// TierVetted is curated, pinned third-party code from a known publisher:
	// the catalog's "official" entries (the service's own vendor or the MCP
	// steering group).
	TierVetted Tier = "vetted"
	// TierThirdParty is everything else: community catalog entries, a custom
	// server an operator adds, a server a model registers, unknown code.
	TierThirdParty Tier = "third-party"
)

// Tiers lists every launch tier.
var Tiers = []Tier{TierFirstParty, TierVetted, TierThirdParty}

// ParseTier validates a tier name.
func ParseTier(s string) (Tier, bool) {
	for _, t := range Tiers {
		if string(t) == s {
			return t, true
		}
	}
	return "", false
}

// Control is one containment property.
type Control string

// The controls a tier can want.
const (
	ControlEnv          Control = "env"           // deny-by-default environment (procenv)
	ControlWorkdir      Control = "workdir"       // dedicated working directory, its own TEMP
	ControlOutputCap    Control = "output_cap"    // one stdout message larger than the cap kills the process
	ControlStallKill    Control = "stall_kill"    // a request unanswered for stall_timeout kills the process
	ControlKillTree     Control = "kill_tree"     // stopping the server stops every process it started
	ControlMemory       Control = "memory"        // memory ceiling
	ControlProcesses    Control = "processes"     // process-count ceiling (fork bomb)
	ControlCPUTime      Control = "cpu_time"      // cumulative CPU-time ceiling
	ControlFSConfine    Control = "fs_confine"    // writes only under the workdir / declared root
	ControlNetwork      Control = "network"       // egress off unless declared
	ControlLowPrivilege Control = "low_privilege" // a lower-privilege identity than the kernel's
)

// on_unavailable values.
const (
	OnUnavailableRefuse  = "refuse"
	OnUnavailableDegrade = "degrade"
)

// Runners.
const (
	RunnerNative = "native"
	RunnerDocker = "docker"
)

// Network declarations.
const (
	NetworkNone   = "none"   // no egress (the default for a server that declares nothing)
	NetworkEgress = "egress" // the server needs the network (hosts are recorded)
)

// TierPolicy is the containment one tier wants (config mcp.sandbox.tiers.<tier>).
type TierPolicy struct {
	// OnUnavailable: "refuse" (do not start when a wanted control cannot be
	// enforced on this platform) or "degrade" (start and report it).
	OnUnavailable string
	// Runner: "native" or "docker". A degrade tier whose docker runner is
	// unavailable falls back to native; a refuse tier does not.
	Runner string
	// ConfineFS / DenyNetwork / LowPrivilege are the third-party controls.
	ConfineFS    bool
	DenyNetwork  bool
	LowPrivilege bool
	// Limits. 0 disables that one limit (and the control is then not wanted).
	MemoryMB        int
	MaxProcesses    int
	CPUTimeSec      int
	StallTimeoutSec int
	MaxMessageKB    int
}

// ServerSpec is what one server declares about its own containment (config
// server `sandbox`, or its catalog entry's).
type ServerSpec struct {
	// Network is "none" (default) or "egress". Hosts are the hosts an egress
	// server needs; recorded and reported, NOT filtered (no egress proxy yet).
	Network string
	Hosts   []string
	// Root is an extra writable directory for a confined server (absolute).
	Root string
	// Runner overrides the tier's runner; Image picks the docker image.
	Runner string
	Image  string
	// MemoryMB / MaxProcesses override the tier's limits for this server.
	MemoryMB     int
	MaxProcesses int
}

// CatalogEntry is the containment-relevant part of one integration-catalog
// entry, keyed by catalog id in Policy.Catalog.
type CatalogEntry struct {
	TrustTier string // the catalog's own vocabulary (official | community)
	Server    ServerSpec
}

// Policy is the whole sandbox configuration, resolved once at the composition
// root from config mcp.sandbox plus the integration catalog. Immutable after
// construction; shared by pointer.
type Policy struct {
	// Disabled turns containment off (env allowlist only). An explicit
	// operator choice, logged at WARN on every launch.
	Disabled bool
	// DefaultTier is the tier of a server that declares none and has no
	// catalog entry.
	DefaultTier Tier
	// CatalogTiers maps the catalog's trust_tier vocabulary to launch tiers.
	CatalogTiers map[string]Tier
	// Catalog is the loaded integration catalog's containment view.
	Catalog map[string]CatalogEntry
	// Tiers is each tier's containment policy.
	Tiers map[Tier]TierPolicy

	// WorkRoot holds each server's working directory: <WorkRoot>/<tier>/<id>.
	WorkRoot string
	// WorkdirEnv are variables pointed at a workdir subdirectory on EVERY
	// launch (TEMP/TMP); ConfinedEnv additionally on a confined launch (package
	// caches, which a write-confined process cannot reach in the user profile).
	WorkdirEnv  map[string]string
	ConfinedEnv map[string]string

	BwrapPath    string            // "" ⇒ look up "bwrap" on PATH
	DockerPath   string            // "" ⇒ look up "docker" on PATH
	DockerImages map[string]string // launch command basename (npx, uvx, python…) → image
	// HidePaths are masked from a bubblewrap-confined process (the kernel's
	// config and data directories: secret.key, the bbolt store).
	HidePaths []string
	// CgroupParent is a delegated cgroup v2 directory (Linux) under which each
	// server gets its own cgroup (memory.max, pids.max). "" ⇒ none.
	CgroupParent string
}

// DefaultPolicy is the built-in policy: the documented config defaults. It is
// what an UNRESOLVED Spec launches under, so a launch path that forgot to
// resolve its policy gets the strict default tier, never no containment.
func DefaultPolicy() *Policy {
	return &Policy{
		DefaultTier: TierThirdParty,
		CatalogTiers: map[string]Tier{
			"official":  TierVetted,
			"curated":   TierVetted, // reviewed + pinned community server (catalog.TrustCurated)
			"community": TierThirdParty,
		},
		Tiers: map[Tier]TierPolicy{
			TierFirstParty: {OnUnavailable: OnUnavailableDegrade, Runner: RunnerNative,
				MemoryMB: 4096, MaxProcesses: 128, CPUTimeSec: 0, StallTimeoutSec: 900, MaxMessageKB: 32768},
			TierVetted: {OnUnavailable: OnUnavailableDegrade, Runner: RunnerNative,
				MemoryMB: 4096, MaxProcesses: 128, CPUTimeSec: 0, StallTimeoutSec: 900, MaxMessageKB: 32768},
			TierThirdParty: {OnUnavailable: OnUnavailableRefuse, Runner: RunnerNative,
				ConfineFS: true, DenyNetwork: true, LowPrivilege: true,
				MemoryMB: 2048, MaxProcesses: 64, CPUTimeSec: 3600, StallTimeoutSec: 600, MaxMessageKB: 16384},
		},
		WorkRoot:   defaultWorkRoot(),
		WorkdirEnv: map[string]string{"TEMP": "tmp", "TMP": "tmp", "TMPDIR": "tmp"},
		ConfinedEnv: map[string]string{
			"HOME": "home", "XDG_CACHE_HOME": "cache", "npm_config_cache": "cache/npm",
			// uvx takes a lock under %APPDATA%\uv\tools (found live:
			// "failed to open ...\uv\tools\.lock: access denied" at low
			// integrity), so its tool dir moves into the workdir too.
			"UV_CACHE_DIR": "cache/uv", "UV_TOOL_DIR": "uv-tools", "PIP_CACHE_DIR": "cache/pip",
		},
		DockerImages: map[string]string{
			"npx": "node:22-bookworm-slim", "node": "node:22-bookworm-slim",
			"uvx": "ghcr.io/astral-sh/uv:python3.12-bookworm-slim", "uv": "ghcr.io/astral-sh/uv:python3.12-bookworm-slim",
			"python": "python:3.12-slim-bookworm", "python3": "python:3.12-slim-bookworm",
		},
	}
}

func defaultWorkRoot() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "cambrian", "sandbox")
	}
	return filepath.Join(os.TempDir(), "cambrian-sandbox")
}

// Spec is one server's resolved launch policy. It travels with the server's
// connection config so every launch path (boot, runtime save, console probe)
// launches under the same policy.
type Spec struct {
	ServerID string
	Tier     Tier
	// TierSource says where the tier came from: "config", "catalog:<id>",
	// "plugin", "default".
	TierSource string
	Server     ServerSpec
	Policy     *Policy
}

// Resolved reports whether the spec came through Policy.Resolve.
func (s Spec) Resolved() bool { return s.Policy != nil && s.Tier != "" }

// Resolve builds a server's Spec. declaredTier is the server config's own
// trust_tier ("" if none); catalogID its catalog provenance; plugin marks a
// plugin-contributed server. server is what the config declares; a field it
// leaves unset is taken from the catalog entry's declaration.
func (p *Policy) Resolve(serverID, declaredTier, catalogID string, plugin bool, server ServerSpec) Spec {
	if p == nil {
		p = DefaultPolicy()
	}
	spec := Spec{ServerID: serverID, Server: server, Policy: p}
	entry, inCatalog := p.Catalog[catalogID]
	switch {
	case declaredTier != "":
		if t, ok := ParseTier(declaredTier); ok {
			spec.Tier, spec.TierSource = t, "config"
		} else {
			// Validation refuses this at load; a stored value that got past it
			// falls to the strictest tier rather than the default.
			spec.Tier, spec.TierSource = TierThirdParty, "config(invalid:"+declaredTier+")"
		}
	case catalogID != "" && inCatalog:
		t, ok := p.CatalogTiers[entry.TrustTier]
		if !ok {
			t = TierThirdParty
		}
		spec.Tier, spec.TierSource = t, "catalog:"+catalogID
	case plugin:
		spec.Tier, spec.TierSource = TierFirstParty, "plugin"
	default:
		spec.Tier, spec.TierSource = p.DefaultTier, "default"
		if spec.Tier == "" {
			spec.Tier = TierThirdParty
		}
	}
	if inCatalog {
		spec.Server = mergeServer(server, entry.Server)
	}
	return spec
}

// mergeServer fills the fields a server config leaves unset from its catalog
// entry's declaration (an install made before the catalog declared sandbox
// needs still gets the declaration).
func mergeServer(own, cat ServerSpec) ServerSpec {
	if own.Network == "" {
		own.Network, own.Hosts = cat.Network, cat.Hosts
	}
	if own.Runner == "" {
		own.Runner = cat.Runner
	}
	if own.Image == "" {
		own.Image = cat.Image
	}
	if own.MemoryMB == 0 {
		own.MemoryMB = cat.MemoryMB
	}
	if own.MaxProcesses == 0 {
		own.MaxProcesses = cat.MaxProcesses
	}
	return own
}

// effective returns the spec's policy pointer, the built-in default when the
// spec was never resolved (fail closed: the default tier, built-in limits).
func (s Spec) effective() (Spec, *Policy) {
	if s.Resolved() {
		return s, s.Policy
	}
	p := DefaultPolicy()
	r := p.Resolve(s.ServerID, "", "", false, s.Server)
	r.TierSource = "unresolved"
	return r, p
}

// tierPolicy returns the tier's policy with the server's overrides applied.
func (s Spec) tierPolicy(p *Policy) TierPolicy {
	tp, ok := p.Tiers[s.Tier]
	if !ok {
		tp = DefaultPolicy().Tiers[TierThirdParty]
	}
	if s.Server.MemoryMB > 0 {
		tp.MemoryMB = s.Server.MemoryMB
	}
	if s.Server.MaxProcesses > 0 {
		tp.MaxProcesses = s.Server.MaxProcesses
	}
	if s.Server.Runner != "" {
		tp.Runner = s.Server.Runner
	}
	if tp.Runner == "" {
		tp.Runner = RunnerNative
	}
	if tp.OnUnavailable == "" {
		tp.OnUnavailable = OnUnavailableRefuse
	}
	return tp
}

// wanted lists the controls a tier policy asks for, in report order.
func (tp TierPolicy) wanted() []Control {
	w := []Control{ControlEnv, ControlWorkdir, ControlKillTree}
	if tp.MaxMessageKB > 0 {
		w = append(w, ControlOutputCap)
	}
	if tp.StallTimeoutSec > 0 {
		w = append(w, ControlStallKill)
	}
	if tp.MemoryMB > 0 {
		w = append(w, ControlMemory)
	}
	if tp.MaxProcesses > 0 {
		w = append(w, ControlProcesses)
	}
	if tp.CPUTimeSec > 0 {
		w = append(w, ControlCPUTime)
	}
	if tp.ConfineFS {
		w = append(w, ControlFSConfine)
	}
	if tp.DenyNetwork {
		w = append(w, ControlNetwork)
	}
	if tp.LowPrivilege {
		w = append(w, ControlLowPrivilege)
	}
	return w
}

// Report is one launch's effective containment: what the console shows.
type Report struct {
	ServerID   string
	Tier       Tier
	TierSource string
	Runner     string
	// Mechanisms name what enforced it: job_object, low_integrity, ulimit,
	// bwrap, cgroup, uid, docker, process_group.
	Mechanisms []string
	Enforced   []Control
	// Missing are wanted controls this platform could not enforce,
	// "control: reason".
	Missing []string
	// Notes are honest caveats that are not missing controls (egress hosts are
	// recorded but not filtered; reads are not confined on Windows).
	Notes    []string
	Refused  bool
	Disabled bool
}

// Degraded reports whether the server runs with a wanted control missing.
func (r Report) Degraded() bool { return !r.Refused && len(r.Missing) > 0 }

// Summary is the one-line containment string for status surfaces:
//
//	containment third-party/native: job_object+low_integrity [env workdir …]; missing: network (…)
func (r Report) Summary() string {
	if r.Disabled {
		return "containment disabled (mcp.sandbox.enabled=false): env allowlist only"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "containment %s/%s", r.Tier, r.Runner)
	if len(r.Mechanisms) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(r.Mechanisms, "+"))
	}
	if len(r.Enforced) > 0 {
		names := make([]string, len(r.Enforced))
		for i, c := range r.Enforced {
			names[i] = string(c)
		}
		fmt.Fprintf(&b, " [%s]", strings.Join(names, " "))
	}
	switch {
	case r.Refused:
		fmt.Fprintf(&b, "; REFUSED, missing: %s", strings.Join(r.Missing, "; "))
	case len(r.Missing) > 0:
		fmt.Fprintf(&b, "; degraded, missing: %s", strings.Join(r.Missing, "; "))
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "; notes: %s", strings.Join(r.Notes, "; "))
	}
	return b.String()
}

// RefusedError is returned when a tier refuses to start without a control.
type RefusedError struct{ Report Report }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("sandbox refused to start %q at tier %s (on_unavailable=refuse): missing %s",
		e.Report.ServerID, e.Report.Tier, strings.Join(e.Report.Missing, "; "))
}

// capability is what one runner on this platform can enforce right now.
type capability struct {
	runner     string
	enforced   map[Control]string // control → mechanism
	missing    map[Control]string // control → reason
	notes      []string
	unusable   string // non-empty: the runner cannot run at all here (reason)
	mechanisms []string
}

// plan resolves the controls a spec wants against a runner's capability.
func plan(spec Spec, tp TierPolicy, c capability) Report {
	r := Report{ServerID: spec.ServerID, Tier: spec.Tier, TierSource: spec.TierSource, Runner: c.runner}
	mech := map[string]bool{}
	for _, w := range tp.wanted() {
		if w == ControlNetwork && spec.Server.Network == NetworkEgress {
			// Declared egress is a grant, not a missing control: the operator or
			// the catalog said this server needs the network. Checked first so a
			// runner that COULD deny the network is not reported as denying it.
			r.Notes = append(r.Notes, egressNote(spec.Server.Hosts))
			continue
		}
		if m, ok := c.enforced[w]; ok {
			r.Enforced = append(r.Enforced, w)
			if m != "" {
				mech[m] = true
			}
			continue
		}
		reason := c.missing[w]
		if reason == "" {
			reason = "not available on this platform"
		}
		r.Missing = append(r.Missing, string(w)+": "+reason)
	}
	r.Notes = append(r.Notes, c.notes...)
	for m := range mech {
		r.Mechanisms = append(r.Mechanisms, m)
	}
	sort.Strings(r.Mechanisms)
	if len(r.Missing) > 0 && tp.OnUnavailable == OnUnavailableRefuse {
		r.Refused = true
	}
	return r
}

func egressNote(hosts []string) string {
	if len(hosts) == 0 {
		return "network: egress declared (no host list)"
	}
	return "network: egress declared for " + strings.Join(hosts, ",") + " (hosts recorded, not filtered)"
}
