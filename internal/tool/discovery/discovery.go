// Package discovery auto-discovers system tools from tools/*tool.py manifests
// into the kernel-owned ToolRegistry (ADR-0039 A1.1) — replacing hand-written Go
// registration. A tool file declares a TOOL_MANIFEST triple-quoted JSON literal
// (mirroring AGENT_MANIFEST), parsed before the Python process is ever booted.
package discovery

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

var toolManifestRegex = regexp.MustCompile(`(?s)TOOL_MANIFEST\s*=\s*'''([\s\S]*?)'''`)

// toolManifest is the JSON contract a tool file declares. The manifest is NOT a
// trust input for the resource policy (A1.5): it declares schema/kind/dangerous
// and which args are paths/urls/commands; the policy bounds come from the grant.
type toolManifest struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Dangerous      bool            `json:"dangerous"`
	PathArgs       []string        `json:"path_args"`
	URLArgs        []string        `json:"url_args"`
	CommandArgs    []string        `json:"command_args"`
	DataReadKinds  []string        `json:"data_read_kinds"`
	DataWriteKinds []string        `json:"data_write_kinds"`
	Schema         json.RawMessage `json:"schema"`

	// ClassificationTags name the domain this tool touches (ADR-0085 D2); Effects
	// are the closed-set verb classes it exercises (ADR-0086). A manifest that
	// omits effects has them inferred from its other fields, unless the deployment
	// runs strict — see domain.ValidateRegistration.
	ClassificationTags []string `json:"classification_tags"`
	Effects            []string `json:"effects"`
}

// Discovered pairs a parsed tool with the path of the *tool.py that declared it
// (the ProcessHandler needs the path to invoke the module).
type Discovered struct {
	Tool domain.SystemTool
	File string
}

// ScanTools reads every *tool.py in dir and returns the parsed SystemTools.
func ScanTools(dir string) ([]domain.SystemTool, error) {
	d, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SystemTool, len(d))
	for i, x := range d {
		out[i] = x.Tool
	}
	return out, nil
}

// Discover reads every *tool.py in dir, extracts its TOOL_MANIFEST, and returns
// the parsed tools with their file paths. A missing dir yields zero tools (not
// an error). A file without a manifest or with malformed JSON is skipped with a
// warning — one bad tool file must not break discovery of the rest.
func Discover(dir string) ([]Discovered, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan tools: %w", err)
	}

	var tools []Discovered
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "tool.py") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			slog.Warn("tool discovery: read failed", "file", e.Name(), "err", rerr)
			continue
		}
		m := toolManifestRegex.FindSubmatch(content)
		if m == nil {
			slog.Warn("tool discovery: no TOOL_MANIFEST, skipping", "file", e.Name())
			continue
		}
		// A manifest is either a single tool object, or {"tools": [ ... ]} for an
		// impl file that serves several tools (e.g. file: read/write/patch/search).
		var multi struct {
			Tools []toolManifest `json:"tools"`
		}
		mans := []toolManifest{}
		if jerr := json.Unmarshal(m[1], &multi); jerr == nil && len(multi.Tools) > 0 {
			mans = multi.Tools
		} else {
			var man toolManifest
			if jerr := json.Unmarshal(m[1], &man); jerr != nil {
				slog.Warn("tool discovery: malformed TOOL_MANIFEST, skipping", "file", e.Name(), "err", jerr)
				continue
			}
			mans = []toolManifest{man}
		}
		for _, man := range mans {
			if strings.TrimSpace(man.Name) == "" {
				slog.Warn("tool discovery: manifest entry missing name, skipping", "file", e.Name())
				continue
			}
			tools = append(tools, Discovered{
				File: path,
				Tool: domain.SystemTool{
					Name:               man.Name,
					Description:        man.Description,
					Schema:             man.Schema,
					Dangerous:          man.Dangerous,
					PathArgs:           man.PathArgs,
					URLArgs:            man.URLArgs,
					CommandArgs:        man.CommandArgs,
					DataReadKinds:      man.DataReadKinds,
					DataWriteKinds:     man.DataWriteKinds,
					ClassificationTags: man.ClassificationTags,
					Effects:            toEffects(man.Effects),
				},
			})
		}
	}
	return tools, nil
}

// toEffects converts the manifest's strings to effect classes WITHOUT validating
// them — validation belongs to domain.ValidateRegistration, which is the single
// place that decides what an unknown or absent effect means.
func toEffects(names []string) []domain.ToolEffect {
	if len(names) == 0 {
		return nil
	}
	out := make([]domain.ToolEffect, 0, len(names))
	for _, n := range names {
		out = append(out, domain.ToolEffect(n))
	}
	return out
}

// LoadRegistry scans dir, registers every discovered tool into reg, and returns
// the tool-name → file-path map the ProcessHandler invokes.
//
// strict refuses to infer effects: an undeclared tool fails registration instead
// of being classified from its other manifest fields (ADR-0086). A tool that
// fails validation is SKIPPED with an error log rather than aborting discovery —
// one bad manifest must not take out every other tool, which is the same posture
// discovery already takes for malformed JSON.
func LoadRegistry(dir string, reg domain.ToolRegistry, strict bool) (map[string]string, error) {
	d, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string, len(d))
	var inferred []string
	for _, x := range d {
		// Validate here so STRICT mode can reject an undeclared tool. The registry
		// validates again (non-strict) — that repetition is deliberate: the registry
		// is the chokepoint no path may skip, and this is where the deployment's
		// strictness is known.
		tool, verr := domain.ValidateRegistration(x.Tool, strict)
		if verr != nil {
			slog.Error("tool discovery: registration rejected", "tool", x.Tool.Name, "file", x.File, "err", verr)
			continue
		}
		if tool.EffectsInferred {
			inferred = append(inferred, tool.Name)
		}
		reg.Register(tool)
		files[tool.Name] = x.File
	}
	if len(inferred) > 0 {
		slog.Warn("ADR-0086: tools registered with INFERRED effect classes; declare them in the manifest",
			"count", len(inferred), "tools", inferred)
	}
	return files, nil
}
