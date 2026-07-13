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
	Description     string          `json:"description"`
	Dangerous       bool            `json:"dangerous"`
	PathArgs        []string        `json:"path_args"`
	URLArgs         []string        `json:"url_args"`
	CommandArgs     []string        `json:"command_args"`
	DataReadKinds   []string        `json:"data_read_kinds"`
	DataWriteKinds  []string        `json:"data_write_kinds"`
	Schema          json.RawMessage `json:"schema"`
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
					Name:           man.Name,
					Description:     man.Description,
					Schema:          man.Schema,
					Dangerous:       man.Dangerous,
					PathArgs:        man.PathArgs,
					URLArgs:         man.URLArgs,
					CommandArgs:     man.CommandArgs,
					DataReadKinds:   man.DataReadKinds,
					DataWriteKinds:  man.DataWriteKinds,
				},
			})
		}
	}
	return tools, nil
}

// LoadRegistry scans dir, registers every discovered tool into reg, and returns
// the tool-name → file-path map the ProcessHandler invokes.
func LoadRegistry(dir string, reg domain.ToolRegistry) (map[string]string, error) {
	d, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string, len(d))
	for _, x := range d {
		reg.Register(x.Tool)
		files[x.Tool.Name] = x.File
	}
	return files, nil
}
