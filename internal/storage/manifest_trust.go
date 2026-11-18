package storage

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
)

// ADR-0139 D8 tiers a local agent's manifest (AGENT_MANIFEST or a
// *.manifest.json) may name in its trust_tier field.
const (
	manifestTierSystem     = "system"
	manifestTierFirstParty = "first-party"
	manifestTierThirdParty = "third-party"
)

// manifestTrustTier is the tier a discovered local agent registers at, from its
// manifest's trust_tier (owner decision 2026-11-18, ENG-2085, option a): a
// manifest may LOWER privilege, never raise it.
//
//   - a regular agent: "" (no declaration; the kernel's first-party default),
//     "first-party" and "third-party" are applied as written;
//   - a composition-root system agent (system): "" and "system" are accepted —
//     the tier is the composition root's grant, never the manifest's.
//
// Anything else is refused (ok=false: the agent is not registered) with the
// field named. That covers "system" on a regular agent (a manifest cannot grant
// itself system status), an unknown value, and a lower tier on a system organ
// (the launcher runs a system organ as system whatever its manifest says, so the
// declaration could only be dropped — the silent fail-open this replaces).
//
// Refusal over warn-and-ignore: a security declaration the kernel will not apply
// as written must not register an agent that runs with more privilege than its
// author asked for. No shipped agent declares the field (checked 2026-11-18).
func manifestTrustTier(id, file, declared string, system bool) (string, bool) {
	refuse := func(why string) (string, bool) {
		slog.Error("agent manifest trust_tier refused: agent NOT registered (ADR-0139 D8, ENG-2085: a manifest may lower privilege, never raise it)",
			"id", id, "file", file, "field", "trust_tier", "value", declared, "why", why)
		return "", false
	}
	if system {
		switch declared {
		case "", manifestTierSystem:
			return declared, true
		case manifestTierFirstParty, manifestTierThirdParty:
			return refuse("the composition root runs this agent as system; a system organ cannot declare a lower tier")
		}
		return refuse("unknown tier (system agent: omit the field or say \"system\")")
	}
	switch declared {
	case "", manifestTierFirstParty, manifestTierThirdParty:
		return declared, true
	case manifestTierSystem:
		return refuse("a manifest cannot grant system status; only the composition root can")
	}
	return refuse("unknown tier (want \"first-party\" or \"third-party\")")
}

// refuseUnparsableManifest logs the ERROR for an agent whose manifest is present
// but cannot be parsed and reports the refusal (owner decision 2026-11-18,
// ENG-2094). It returns false so a record builder can `return
// DiscoveredAgent{}, refuseUnparsableManifest(...)`.
//
// Refusal over the old empty-manifest-plus-WARN: an empty manifest drops every
// declaration in it, trust_tier included, so a "third-party" agent whose JSON
// had one typo ran first-party — the fail-open ENG-2085 closed for well-formed
// manifests (AGENTS.md §3.2: access decisions fail closed). An agent with NO
// manifest is unaffected. No shipped agent's manifest fails to parse (checked
// 2026-11-18: core, premium, plugins, SDK examples, benchmark fixtures).
func refuseUnparsableManifest(id, file string, err error, refused func(string)) bool {
	slog.Error("agent manifest is present but unparsable: agent NOT registered and any earlier registration dropped (ENG-2094)",
		"id", id, "file", file, "err", err)
	refused(id)
	return false
}

// siblingManifest reads <dir>/<id>.manifest.json, the canonical manifest of a
// source-file agent (ADR-0075 sidecar preference). present=false means there is
// no such file; err is set when the file is present but unreadable or not valid
// JSON (the caller refuses the agent, ENG-2094).
func siblingManifest(path string) (m ManifestRecord, present bool, err error) {
	b, readErr := os.ReadFile(path)
	if errors.Is(readErr, fs.ErrNotExist) {
		return ManifestRecord{}, false, nil
	}
	if readErr != nil {
		return ManifestRecord{}, true, readErr
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return ManifestRecord{}, true, err
	}
	return m, true, nil
}
