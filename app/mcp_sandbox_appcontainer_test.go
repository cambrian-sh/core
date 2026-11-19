package app

import (
	"slices"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2215: mcp.sandbox.appcontainer_reads overlays the defaults (keys
// lower-cased; a given key replaces its default, an empty list clears it).
func TestSandboxPolicyFromConfig_AppContainerReads(t *testing.T) {
	def := sandboxPolicyFromConfig(config.MCPSandboxConfig{}, nil, nil)
	if want := sandbox.DefaultPolicy().AppContainerReads["uvx"]; len(want) == 0 || !slices.Equal(def.AppContainerReads["uvx"], want) {
		t.Fatalf("defaults: %v", def.AppContainerReads)
	}
	p := sandboxPolicyFromConfig(config.MCPSandboxConfig{AppContainerReads: map[string][]string{
		"UVX": {`D:\pythons`}, "uv": {}, "pixi": {`${HOME}\.pixi`},
	}}, nil, nil)
	if !slices.Equal(p.AppContainerReads["uvx"], []string{`D:\pythons`}) || len(p.AppContainerReads["uv"]) != 0 ||
		!slices.Equal(p.AppContainerReads["pixi"], []string{`${HOME}\.pixi`}) {
		t.Fatalf("overlay: %v", p.AppContainerReads)
	}
}
