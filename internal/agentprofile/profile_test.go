package agentprofile

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
)

var updateProfileCases = flag.Bool("update-profile-cases", false,
	"rewrite testdata/profile_cases.json from the Go validator")

const baseProfile = `# a comment
name: tester
version: 1.2.3
description: Tests things. Second sentence.
role: "You test: carefully."
capabilities:
  - analysis
  - file_read
skills:
  - alpha
tools:
  - mcp:filesystem/read_*
  - mcp:time/*
trust_tier: first-party
max_tool_rounds: 8
`

// fixtureSkills is the skill index every fixture case validates against.
var fixtureSkills = map[string][]string{
	"alpha": {"mcp:filesystem/read_text_file"},
	"beta":  {"mcp:shell/run"},
	"gamma": {},
}

// profileCase is one row of the cross-language fixture.
type profileCase struct {
	Name         string              `json:"name"`
	Text         string              `json:"text"`
	ExpectedName string              `json:"expected_name"`
	Skills       map[string][]string `json:"skills"` // null ⇒ skill checks skipped
	Valid        bool                `json:"valid"`
	Profile      *Profile            `json:"profile,omitempty"`
	Warnings     int                 `json:"warnings"`
}

type globCase struct {
	Pattern string `json:"pattern"`
	Name    string `json:"name"`
	Want    bool   `json:"want"`
}

type fixture struct {
	Constants map[string]any `json:"constants"`
	Cases     []profileCase  `json:"cases"`
	Globs     []globCase     `json:"globs"`
}

func with(old, new string) string {
	if !strings.Contains(baseProfile, old) {
		panic("fixture mutation does not apply: " + old)
	}
	return strings.Replace(baseProfile, old, new, 1)
}

// rawCases are the inputs; the fixture adds Go's verdict to each.
func rawCases(t *testing.T) []profileCase {
	t.Helper()
	cases := []profileCase{
		{Name: "base", Text: baseProfile, ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "base-crlf", Text: strings.ReplaceAll(baseProfile, "\n", "\r\n"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "no-skill-check", Text: with("  - alpha", "  - nonexistent"), ExpectedName: "tester"},
		{Name: "missing-skill", Text: with("  - alpha", "  - nonexistent"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "skill-tools-outside-allowlist", Text: with("  - alpha", "  - beta"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "skill-without-tools", Text: with("  - alpha", "  - gamma"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "file-name-mismatch", Text: baseProfile, ExpectedName: "other", Skills: fixtureSkills},
		{Name: "unknown-field", Text: baseProfile + "extra: 1\n", ExpectedName: "tester"},
		{Name: "duplicate-key", Text: baseProfile + "role: again\n", ExpectedName: "tester"},
		{Name: "missing-role", Text: with("role: \"You test: carefully.\"\n", ""), ExpectedName: "tester"},
		{Name: "bare-key-is-missing", Text: with("role: \"You test: carefully.\"", "role:"), ExpectedName: "tester"},
		{Name: "unquoted-colon-space", Text: with("role: \"You test: carefully.\"", "role: You test: carefully."), ExpectedName: "tester"},
		{Name: "unquoted-hash", Text: with("role: \"You test: carefully.\"", "role: You test #1"), ExpectedName: "tester"},
		{Name: "single-quoted", Text: with("role: \"You test: carefully.\"", "role: 'It''s fine: really'"), ExpectedName: "tester"},
		{Name: "anchor", Text: with("role: \"You test: carefully.\"", "role: &a x"), ExpectedName: "tester"},
		{Name: "multiline-block", Text: with("role: \"You test: carefully.\"", "role: |"), ExpectedName: "tester"},
		{Name: "bad-indent", Text: with("  - analysis", "  analysis"), ExpectedName: "tester"},
		{Name: "flow-lists", Text: with("capabilities:\n  - analysis\n  - file_read\n", "capabilities: [analysis, file_read]\n"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "flow-list-trailing-comma", Text: with("capabilities:\n  - analysis\n  - file_read\n", "capabilities: [analysis, ]\n"), ExpectedName: "tester"},
		{Name: "unterminated-flow", Text: with("capabilities:\n  - analysis\n  - file_read\n", "capabilities: [analysis\n"), ExpectedName: "tester"},
		{Name: "inline-object", Text: with("trust_tier: first-party", "trust_tier: {\"a\": 1}"), ExpectedName: "tester"},
		{Name: "duplicate-capability", Text: with("  - file_read", "  - analysis"), ExpectedName: "tester"},
		{Name: "camel-capability", Text: with("  - file_read", "  - fileRead"), ExpectedName: "tester"},
		{Name: "reserved-capability", Text: with("  - file_read", "  - conversation"), ExpectedName: "tester"},
		{Name: "new-capability-warns", Text: with("  - file_read", "  - network_analysis"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "int-capability", Text: with("  - file_read", "  - 7"), ExpectedName: "tester"},
		{Name: "bad-skill-name", Text: with("  - alpha", "  - Alpha"), ExpectedName: "tester"},
		{Name: "bad-name", Text: with("name: tester", "name: Tester"), ExpectedName: "Tester"},
		{Name: "name-leading-digit", Text: with("name: tester", "name: 1tester"), ExpectedName: "1tester"},
		{Name: "quote-in-description", Text: with("Tests things.", "Tests \\\"things\\\"."), ExpectedName: "tester"},
		{Name: "bad-trust-tier", Text: with("trust_tier: first-party", "trust_tier: trusted"), ExpectedName: "tester"},
		{Name: "system-trust-tier", Text: with("trust_tier: first-party", "trust_tier: system"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "model-generator", Text: baseProfile + "model: haiku-local\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "model-role", Text: baseProfile + "model: role:planner\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "model-bad-role", Text: baseProfile + "model: role:interview\n", ExpectedName: "tester"},
		{Name: "model-int", Text: baseProfile + "model: 5\n", ExpectedName: "tester"},
		{Name: "model-null", Text: baseProfile + "model: ~\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "bad-version", Text: with("version: 1.2.3", "version: 1.2"), ExpectedName: "tester"},
		{Name: "no-version", Text: with("version: 1.2.3\n", ""), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "rounds-too-high", Text: with("max_tool_rounds: 8", "max_tool_rounds: 65"), ExpectedName: "tester"},
		{Name: "rounds-zero", Text: with("max_tool_rounds: 8", "max_tool_rounds: 0"), ExpectedName: "tester"},
		{Name: "rounds-string", Text: with("max_tool_rounds: 8", "max_tool_rounds: \"8\""), ExpectedName: "tester"},
		{Name: "rounds-null", Text: with("max_tool_rounds: 8", "max_tool_rounds: null"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "max-tokens-ok", Text: baseProfile + "max_tokens: 4096\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "max-tokens-low", Text: baseProfile + "max_tokens: 100\n", ExpectedName: "tester"},
		{Name: "max-tokens-huge", Text: baseProfile + "max_tokens: 99999999999999999999999\n", ExpectedName: "tester"},
		{Name: "glob-wildcard-server", Text: with("mcp:time/*", "mcp:*/read_*"), ExpectedName: "tester"},
		{Name: "glob-class", Text: with("mcp:time/*", "mcp:time/[ab]"), ExpectedName: "tester"},
		{Name: "glob-doublestar", Text: with("mcp:time/*", "mcp:time/**"), ExpectedName: "tester"},
		{Name: "glob-bare-star", Text: with("mcp:time/*", "'*'"), ExpectedName: "tester"},
		{Name: "glob-native", Text: with("mcp:time/*", "web_search"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "glob-native-wild", Text: with("mcp:time/*", "web_*"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "glob-other-namespace", Text: with("mcp:time/*", "local:box/read"), ExpectedName: "tester"},
		{Name: "glob-uppercase-server", Text: with("mcp:time/*", "mcp:Time/now"), ExpectedName: "tester"},
		{Name: "glob-whitespace", Text: with("mcp:time/*", "'mcp:time/ x'"), ExpectedName: "tester"},
		{Name: "duplicate-tool", Text: with("mcp:time/*", "mcp:filesystem/read_*"), ExpectedName: "tester"},
		{Name: "empty-tools", Text: with("tools:\n  - mcp:filesystem/read_*\n  - mcp:time/*\n", "tools: []\n"), ExpectedName: "tester"},
		{Name: "comment-indented", Text: with("skills:\n", "skills:\n    # inside\n"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "bom", Text: "ï»¿" + baseProfile, ExpectedName: "tester"},
	}
	// The shipped profiles, against the shipped skills (the kernel's view).
	shipped := shippedSkillTools(t)
	paths, _ := filepath.Glob(filepath.Join("..", "..", "agents", "profiles", "*.yaml"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		stem := strings.TrimSuffix(filepath.Base(p), ".yaml")
		cases = append(cases, profileCase{Name: "shipped-" + stem, Text: string(b), ExpectedName: stem, Skills: shipped})
	}
	return cases
}

func shippedSkillTools(t *testing.T) map[string][]string {
	t.Helper()
	skills, err := skilldiscovery.Discover(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, s := range skills {
		out[s.Name] = append([]string{}, s.ToolGrants...)
	}
	return out
}

var rawGlobs = []globCase{
	{"mcp:filesystem/read_*", "mcp:filesystem/read_text_file", false},
	{"mcp:filesystem/read_*", "mcp:filesystem/write_file", false},
	{"mcp:filesystem/read_*", "mcp:filesystem/read_", false},
	{"mcp:fetch/*", "mcp:fetch/fetch", false},
	{"mcp:fetch/*", "mcp:fetch/a/b", false},
	{"mcp:fetch/*", "mcp:fetchx/fetch", false},
	{"mcp:time/get_?", "mcp:time/get_x", false},
	{"mcp:time/get_?", "mcp:time/get_", false},
	{"mcp:time/get_?", "mcp:time/get_xy", false},
	{"web_*", "web_search", false},
	{"web_*", "Web_search", false},
	{"a*b*c", "abxbxc", false},
	{"a*b*c", "abxbx", false},
	{"*", "", false},
	{"mcp:x/a\\b", "mcp:x/a\\b", false},
	{"mcp:x/é*", "mcp:x/éa", false},
}

func buildFixture(t *testing.T) fixture {
	t.Helper()
	fx := fixture{Constants: map[string]any{
		"capability_vocabulary": sorted(CapabilityVocabulary),
		"reserved_capabilities": sorted(ReservedCapabilities),
		"trust_tiers":           TrustTiers,
		"model_roles":           ModelRoles,
		"fields":                sorted(Fields),
		"required":              Required,
		"max_tool_rounds":       MaxToolRoundsBounds,
		"max_tokens":            MaxTokensBounds,
		"default_version":       DefaultVersion,
	}}
	for _, c := range rawCases(t) {
		p, err := Parse(c.Text, Options{ExpectedName: c.ExpectedName, SkillTools: c.Skills})
		c.Valid = err == nil
		if c.Valid {
			pp := p
			c.Profile = &pp
			c.Warnings = len(p.Warnings)
		}
		fx.Cases = append(fx.Cases, c)
	}
	for _, g := range rawGlobs {
		g.Want = domain.MatchToolGlob(g.Pattern, g.Name)
		fx.Globs = append(fx.Globs, g)
	}
	return fx
}

// TestProfileCasesFixture pins testdata/profile_cases.json to the Go validator.
// sdk/tests/test_profile_parity.py holds the SDK's parse_profile to the same
// file, so the kernel registers exactly the profiles the runtime accepts.
// Regenerate with: go test ./internal/agentprofile -run TestProfileCasesFixture -update-profile-cases
func TestProfileCasesFixture(t *testing.T) {
	fx := buildFixture(t)
	want, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("testdata", "profile_cases.json")
	if *updateProfileCases {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (regenerate with -update-profile-cases): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("testdata/profile_cases.json is stale; regenerate with -update-profile-cases")
	}
}

func TestShippedProfilesValidateAgainstShippedSkills(t *testing.T) {
	loaded, err := LoadDir(filepath.Join("..", "..", "agents", "profiles"), shippedSkillTools(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) < 3 {
		t.Fatalf("want the three starter profiles, got %d", len(loaded))
	}
	for _, l := range loaded {
		if l.Err != nil {
			t.Errorf("%s: %v", l.Path, l.Err)
		}
	}
}

func TestParseVerdicts(t *testing.T) {
	// A few verdicts asserted directly, so a regenerated fixture cannot silently
	// flip the ones that matter.
	byName := map[string]profileCase{}
	for _, c := range buildFixture(t).Cases {
		byName[c.Name] = c
	}
	for name, valid := range map[string]bool{
		"base": true, "base-crlf": true, "missing-skill": false, "skill-tools-outside-allowlist": false,
		"unknown-field": false, "unquoted-colon-space": false, "glob-wildcard-server": false,
		"reserved-capability": false, "new-capability-warns": true, "model-role": true, "bom": false,
		"flow-list-trailing-comma": false, "max-tokens-huge": false,
	} {
		if byName[name].Valid != valid {
			t.Errorf("%s: valid=%v, want %v", name, byName[name].Valid, valid)
		}
	}
	if byName["new-capability-warns"].Warnings != 1 {
		t.Errorf("new capability should warn once")
	}
	if p := byName["base"].Profile; p == nil || p.Role != "You test: carefully." || *p.MaxToolRounds != 8 || p.Version != "1.2.3" {
		t.Errorf("base parsed wrong: %+v", p)
	}
}

// The shipped profiles and the skills they list name the curated MCP set in
// configs/mcp.json.example (agent stack v2 step 8): every glob's server id is a
// configured server, a glob over a server that lists its tools matches at least one,
// every tool a listed skill names is one its server lists, and no starter profile
// reaches the shell or terminal servers. A profile cannot reach a server whose id it
// does not name, so a renamed server id would otherwise silently grant nothing.
func TestShippedProfiles_ToolIDsMatchTheCuratedMCPSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "mcp.json.example"))
	if err != nil {
		t.Skipf("no mcp.json.example: %v", err)
	}
	var doc struct {
		MCP struct {
			Servers []struct {
				ID    string `json:"id"`
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	listed := map[string][]string{}
	for _, s := range doc.MCP.Servers {
		names := []string{}
		for _, tl := range s.Tools {
			names = append(names, "mcp:"+s.ID+"/"+tl.Name)
		}
		listed[s.ID] = names
	}
	skills := shippedSkillTools(t)
	loaded, err := LoadDir(filepath.Join("..", "..", "agents", "profiles"), skills)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range loaded {
		if l.Err != nil {
			t.Fatalf("%s: %v", l.Path, l.Err)
		}
		p := l.Profile
		for _, g := range p.Tools {
			sid, _, _ := strings.Cut(strings.TrimPrefix(g, "mcp:"), "/")
			tools, ok := listed[sid]
			if !ok {
				t.Errorf("%s: %s names server %q, which mcp.json.example does not configure", p.Name, g, sid)
				continue
			}
			if sid == "shell" || sid == "terminal" {
				t.Errorf("%s: a starter profile must not reach the %s server (%s)", p.Name, sid, g)
			}
			if len(tools) > 0 && len(domain.ToolAllowlist{g}.Expand(asTools(tools))) == 0 {
				t.Errorf("%s: %s matches none of the tools %s lists", p.Name, g, sid)
			}
		}
		for _, s := range p.Skills {
			for _, tool := range skills[s] {
				sid, _, _ := strings.Cut(strings.TrimPrefix(tool, "mcp:"), "/")
				if tools := listed[sid]; len(tools) > 0 && !contains(tools, tool) {
					t.Errorf("skill %s (profile %s) names %s, which server %s does not list", s, p.Name, tool, sid)
				}
			}
		}
	}
}

func asTools(names []string) []domain.SystemTool {
	out := make([]domain.SystemTool, len(names))
	for i, n := range names {
		out[i] = domain.SystemTool{Name: n}
	}
	return out
}
