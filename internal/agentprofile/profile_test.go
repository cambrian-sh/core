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
		{Name: "glob-other-namespace", Text: with("mcp:time/*", "foo:box/read"), ExpectedName: "tester"},
		// Owner decision 2026-11-15: contributed tools (ADR-0127) may be named.
		{Name: "glob-local-machine", Text: with("mcp:time/*", "local:box/read"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "glob-local-fleet", Text: with("mcp:time/*", "local:*"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "glob-local-any-machine", Text: with("mcp:time/*", "local:*/read_*"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "glob-local-partial-machine-wild", Text: with("mcp:time/*", "local:bo*/read"), ExpectedName: "tester"},
		{Name: "glob-local-no-tool", Text: with("mcp:time/*", "local:box/"), ExpectedName: "tester"},
		{Name: "glob-local-bare", Text: with("mcp:time/*", "local:box"), ExpectedName: "tester"},
		{Name: "glob-uppercase-server", Text: with("mcp:time/*", "mcp:Time/now"), ExpectedName: "tester"},
		{Name: "glob-whitespace", Text: with("mcp:time/*", "'mcp:time/ x'"), ExpectedName: "tester"},
		{Name: "duplicate-tool", Text: with("mcp:time/*", "mcp:filesystem/read_*"), ExpectedName: "tester"},
		{Name: "empty-tools", Text: with("tools:\n  - mcp:filesystem/read_*\n  - mcp:time/*\n", "tools: []\n"), ExpectedName: "tester"},
		{Name: "comment-indented", Text: with("skills:\n", "skills:\n    # inside\n"), ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "bom", Text: "ï»¿" + baseProfile, ExpectedName: "tester"},
		{Name: "examples-ok", Text: baseProfile + "examples:\n  - Summarise this file\n  - \"Compare: a and b\"\n  - '  Padded example  '\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "examples-flow", Text: baseProfile + "examples: [one task, another task]\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "examples-empty-list", Text: baseProfile + "examples: []\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "examples-null", Text: baseProfile + "examples: ~\n", ExpectedName: "tester", Skills: fixtureSkills},
		{Name: "examples-too-many", Text: baseProfile + "examples: [a, b, c, d, e, f]\n", ExpectedName: "tester"},
		{Name: "examples-duplicate", Text: baseProfile + "examples: [a, a]\n", ExpectedName: "tester"},
		{Name: "examples-int", Text: baseProfile + "examples: [7]\n", ExpectedName: "tester"},
		{Name: "examples-blank", Text: baseProfile + "examples:\n  - '   '\n", ExpectedName: "tester"},
		{Name: "examples-scalar", Text: baseProfile + "examples: one task\n", ExpectedName: "tester"},
		// ADR-0139 amendment 2026-11-06: streams_output. The subset has no boolean
		// scalar, so exactly true / false (unquoted or quoted); null is false.
		{Name: "streams-output-true", Text: baseProfile + "streams_output: true\n", ExpectedName: "tester"},
		{Name: "streams-output-false", Text: baseProfile + "streams_output: false\n", ExpectedName: "tester"},
		{Name: "streams-output-quoted", Text: baseProfile + "streams_output: \"true\"\n", ExpectedName: "tester"},
		{Name: "streams-output-null", Text: baseProfile + "streams_output: ~\n", ExpectedName: "tester"},
		{Name: "streams-output-yes", Text: baseProfile + "streams_output: yes\n", ExpectedName: "tester"},
		{Name: "streams-output-int", Text: baseProfile + "streams_output: 1\n", ExpectedName: "tester"},
		{Name: "streams-output-list", Text: baseProfile + "streams_output: [true]\n", ExpectedName: "tester"},
		{Name: "examples-too-long", Text: baseProfile + "examples:\n  - " + strings.Repeat("x", MaxExampleRunes+1) + "\n", ExpectedName: "tester"},
		{Name: "examples-max-length-multibyte", Text: baseProfile + "examples:\n  - " + strings.Repeat("é", MaxExampleRunes) + "\n", ExpectedName: "tester", Skills: fixtureSkills},
		// ADR-0157 D1.2 (ENG-1960): sandbox_paths, one-line JSON [{"path", "write"}]; the
		// path absolute on some OS or ~/; cap and kernel-state checks are boot-time.
		{Name: "sandbox-paths-ok", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/models"}, {"path": "~/cache/hf", "write": true}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-windows", Text: baseProfile + "sandbox_paths: " + `[{"path": "C:\\models"}, {"path": "D:/data", "write": false}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-unc-and-tilde-backslash", Text: baseProfile + "sandbox_paths: " + `[{"path": "\\\\nas\\share"}, {"path": "~\\cache"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-write-null", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a", "write": null}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-empty-list", Text: baseProfile + "sandbox_paths: " + `[]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-null", Text: baseProfile + "sandbox_paths: " + `~` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-relative", Text: baseProfile + "sandbox_paths: " + `[{"path": "models/cache"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-dot-relative", Text: baseProfile + "sandbox_paths: " + `[{"path": "./models"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-tilde-user", Text: baseProfile + "sandbox_paths: " + `[{"path": "~bob/models"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-drive-relative", Text: baseProfile + "sandbox_paths: " + `[{"path": "C:models"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-unknown-key", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a", "mode": "rw"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-write-string", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a", "write": "true"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-write-int", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a", "write": 1}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-duplicate", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a"}, {"path": "/srv/a", "write": true}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-missing-path", Text: baseProfile + "sandbox_paths: " + `[{"write": true}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-path-not-string", Text: baseProfile + "sandbox_paths: " + `[{"path": 7}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-whitespace", Text: baseProfile + "sandbox_paths: " + `[{"path": " /srv/a"}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-mixed", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a"}, "/srv/b"]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-bad-json", Text: baseProfile + "sandbox_paths: " + `[{"path": "/srv/a",}]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-object", Text: baseProfile + "sandbox_paths: " + `{"path": "/srv/a"}` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-scalar", Text: baseProfile + "sandbox_paths: " + `/srv/models` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-flow-strings", Text: baseProfile + "sandbox_paths: " + `[/srv/models]` + "\n", ExpectedName: "tester"},
		{Name: "sandbox-paths-block-strings", Text: baseProfile + "sandbox_paths:\n  - /srv/models\n", ExpectedName: "tester"},
		// ADR-0157 D7.6 (ENG-2009): python_packages, one-line JSON [{"name", "version", "extras"}];
		// exact versions, no URL/path/hash keys, no project twice (PEP 503 normalized).
		{Name: "python-packages-ok", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.9.0"}, {"name": "httpx", "version": "0.27.2", "extras": ["http2"]}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-epoch-local", Text: baseProfile + "python_packages: " + `[{"name": "Foo.Bar_baz", "version": "1!2.0.post1+local.7"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-extras-null", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.9.0", "extras": null}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-empty-list", Text: baseProfile + "python_packages: " + `[]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-null", Text: baseProfile + "python_packages: " + `~` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-range", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": ">=1.9"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-eq-prefix", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "==1.9.0"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-wildcard", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.*"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-version-number", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": 1.9}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-missing-version", Text: baseProfile + "python_packages: " + `[{"name": "polars"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-missing-name", Text: baseProfile + "python_packages: " + `[{"version": "1.9.0"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-bad-name", Text: baseProfile + "python_packages: " + `[{"name": "polars @ https://x/p.whl", "version": "1.9.0"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-url-key", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.9.0", "url": "https://x/p.whl"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-hash-key", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.9.0", "hash": "sha256:00"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-duplicate-normalized", Text: baseProfile + "python_packages: " + `[{"name": "Foo_Bar", "version": "1.0"}, {"name": "foo-bar", "version": "2.0"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-extras-string", Text: baseProfile + "python_packages: " + `[{"name": "httpx", "version": "0.27.2", "extras": "http2"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-extras-bad", Text: baseProfile + "python_packages: " + `[{"name": "httpx", "version": "0.27.2", "extras": ["http 2"]}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-extras-duplicate", Text: baseProfile + "python_packages: " + `[{"name": "httpx", "version": "0.27.2", "extras": ["http2", "HTTP2"]}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-version-newline", Text: baseProfile + "python_packages: " + `[{"name": "polars", "version": "1.9.0\n"}]` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-object", Text: baseProfile + "python_packages: " + `{"name": "polars", "version": "1.9.0"}` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-scalar", Text: baseProfile + "python_packages: " + `polars==1.9.0` + "\n", ExpectedName: "tester"},
		{Name: "python-packages-flow-strings", Text: baseProfile + "python_packages: " + `[polars]` + "\n", ExpectedName: "tester"},
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
	{"local:*", "local:box/read_file", false},
	{"local:*/read_*", "local:box/read_file", false},
	{"local:*/read_*", "local:box/write_file", false},
	{"local:box/*", "local:boxy/read", false},
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
		"max_examples":          MaxExamples,
		"max_example_runes":     MaxExampleRunes,
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
		"examples-ok": true, "examples-flow": true, "examples-empty-list": true, "examples-null": true,
		"examples-too-many": false, "examples-duplicate": false, "examples-int": false, "examples-blank": false,
		"examples-scalar": false, "examples-too-long": false, "examples-max-length-multibyte": true,
		"streams-output-true": true, "streams-output-false": true, "streams-output-quoted": true,
		"streams-output-null": true, "streams-output-yes": false, "streams-output-int": false,
		"streams-output-list": false,
		"sandbox-paths-ok":    true, "sandbox-paths-windows": true, "sandbox-paths-unc-and-tilde-backslash": true,
		"sandbox-paths-write-null": true, "sandbox-paths-empty-list": true, "sandbox-paths-null": true,
		"sandbox-paths-relative": false, "sandbox-paths-dot-relative": false, "sandbox-paths-tilde-user": false,
		"sandbox-paths-drive-relative": false, "sandbox-paths-unknown-key": false, "sandbox-paths-write-string": false,
		"sandbox-paths-write-int": false, "sandbox-paths-duplicate": false, "sandbox-paths-missing-path": false,
		"sandbox-paths-path-not-string": false, "sandbox-paths-whitespace": false, "sandbox-paths-mixed": false,
		"sandbox-paths-bad-json": false, "sandbox-paths-object": false, "sandbox-paths-scalar": false,
		"sandbox-paths-flow-strings": false, "sandbox-paths-block-strings": false,
		"python-packages-ok":                   true,
		"python-packages-epoch-local":          true,
		"python-packages-extras-null":          true,
		"python-packages-empty-list":           true,
		"python-packages-null":                 true,
		"python-packages-range":                false,
		"python-packages-eq-prefix":            false,
		"python-packages-wildcard":             false,
		"python-packages-version-number":       false,
		"python-packages-missing-version":      false,
		"python-packages-missing-name":         false,
		"python-packages-bad-name":             false,
		"python-packages-url-key":              false,
		"python-packages-hash-key":             false,
		"python-packages-duplicate-normalized": false,
		"python-packages-extras-string":        false,
		"python-packages-extras-bad":           false,
		"python-packages-extras-duplicate":     false,
		"python-packages-version-newline":      false,
		"python-packages-object":               false,
		"python-packages-scalar":               false,
		"python-packages-flow-strings":         false,
	} {
		if byName[name].Valid != valid {
			t.Errorf("%s: valid=%v, want %v", name, byName[name].Valid, valid)
		}
	}
	if byName["new-capability-warns"].Warnings != 1 {
		t.Errorf("new capability should warn once")
	}
	if p := byName["streams-output-true"].Profile; p == nil || !p.StreamsOutput {
		t.Errorf("streams_output: true should parse true: %+v", p)
	}
	if p := byName["base"].Profile; p == nil || p.StreamsOutput {
		t.Errorf("streams_output defaults to false: %+v", p)
	}
	if p := byName["sandbox-paths-ok"].Profile; p == nil || len(p.SandboxPaths) != 2 ||
		p.SandboxPaths[0] != (domain.SandboxPath{Path: "/srv/models"}) ||
		p.SandboxPaths[1] != (domain.SandboxPath{Path: "~/cache/hf", Write: true}) {
		t.Errorf("sandbox_paths parsed wrong: %+v", p)
	}
	if p := byName["sandbox-paths-windows"].Profile; p == nil || p.SandboxPaths[0].Path != `C:\models` {
		t.Errorf("a JSON-escaped Windows path must decode to one backslash: %+v", p)
	}
	if p := byName["python-packages-ok"].Profile; p == nil || len(p.PythonPackages) != 2 ||
		p.PythonPackages[0].Spec() != "polars==1.9.0" || p.PythonPackages[1].Spec() != "httpx[http2]==0.27.2" {
		t.Errorf("python_packages parsed wrong: %+v", p)
	}
	if p := byName["base"].Profile; p == nil || p.PythonPackages != nil {
		t.Errorf("python_packages defaults to none: %+v", p)
	}
	if p := byName["base"].Profile; p == nil || p.SandboxPaths != nil {
		t.Errorf("sandbox_paths defaults to none: %+v", p)
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
