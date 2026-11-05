package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/agentprofile"
)

// generic capabilities describe nothing and cannot discriminate between agents.
var genericCaps = map[string]bool{"general_purpose": true, "general-purpose": true}

var manifestRe = regexp.MustCompile(`(?s)AGENT_MANIFEST\s*=\s*(?:'''|""")(.*?)(?:'''|""")`)

// Every shipped agent must declare at least one capability that says what it DOES.
//
// This is not style. The capability vocabulary shown to the planner is built from these
// declarations, and the planner's emitted requirements are dropped when they are not in
// that vocabulary — so a capability no agent declares is a capability the planner cannot
// ask for. When every agent declared only `general_purpose`, the whole capability contract
// silently degraded to unconstrained routing, and a summarisation step was auctioned to
// the calculator.
//
// The failure mode is invisible: nothing errors, routing just gets quietly worse.
func TestShippedAgents_DeclareADiscriminatingCapability(t *testing.T) {
	dir := filepath.Join("..", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("agents directory not present: %v", err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		m := manifestRe.FindSubmatch(raw)
		if m == nil {
			continue // not every file carries a manifest
		}
		var manifest struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(m[1], &manifest); err != nil {
			t.Errorf("%s: manifest is not valid JSON: %v", e.Name(), err)
			continue
		}
		checked++

		specific := 0
		for _, c := range manifest.Capabilities {
			if !genericCaps[strings.ToLower(strings.TrimSpace(c))] {
				specific++
			}
		}
		if specific == 0 {
			t.Errorf("%s declares only generic capabilities (%v) — the router has nothing "+
				"to discriminate on, so this agent can win any task",
				e.Name(), manifest.Capabilities)
		}
	}

	if checked == 0 {
		t.Skip("no agent manifests found")
	}
	t.Logf("checked %d agent manifests", checked)
}

// The same rule for declarative profiles (agent stack v2): since part B the
// dispatchable fleet IS the profiles, so a profile declaring only general_purpose
// would make that agent win any task.
func TestShippedProfiles_DeclareADiscriminatingCapability(t *testing.T) {
	loaded, err := agentprofile.LoadDir(filepath.Join("..", "agents", "profiles"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Skip("no profiles")
	}
	for _, l := range loaded {
		if l.Err != nil {
			t.Errorf("%s: %v", l.Path, l.Err)
			continue
		}
		specific := 0
		for _, c := range l.Profile.Capabilities {
			if !genericCaps[c] {
				specific++
			}
		}
		if specific == 0 {
			t.Errorf("profile %s declares only generic capabilities %v", l.Profile.Name, l.Profile.Capabilities)
		}
	}
}
