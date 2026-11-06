// Package agentprofile parses and validates declarative agent profiles
// (agents/profiles/<name>.yaml; format in agents/profiles/README.md) for the
// kernel's agent discovery.
//
// A profile is one specialist agent written as data, run by the one runtime
// agents/worker_agent.py (owner directive decisions/agent-stack-v2-mcp-tools-and-
// profiles.md D3). The SDK's loader (sdk/cambrian_agent_sdk/profiles.py) is the
// format's reference implementation; this package is its Go port, so the kernel
// registers exactly the profiles the runtime will accept. Parity is held by a
// Go-written fixture (testdata/profile_cases.json, TestProfileCasesFixture) that
// the SDK checks its own validator against (sdk/tests/test_profile_parity.py),
// the pattern the prompt-hash and skill-summary ports already use.
package agentprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// TrustTiers are the ADR-0139 D8 containment tiers a profile may declare.
var TrustTiers = []string{"system", "first-party", "third-party"}

// CapabilityVocabulary is the fleet's declared capability spelling (the SDK's
// CAPABILITY_VOCABULARY). A tag outside it is a warning, not an error: the
// kernel's vocabulary is the union of what agents declare (ADR-0100).
var CapabilityVocabulary = []string{
	"general_purpose",
	"analysis", "data_analysis", "code_execution",
	"research", "web_research",
	"file_read", "file_write",
	"summarisation",
}

// ReservedCapabilities belong to the chat front desk; a profile may not declare them.
var ReservedCapabilities = []string{"conversation", "chat"}

// ModelRoles are the generator roles a `model: role:<name>` preference may name.
var ModelRoles = []string{"planner", "verifier", "router", "memory", "facts"}

// Fields are the profile's keys; any other key is an error.
var Fields = []string{
	"name", "description", "capabilities", "skills", "tools", "model", "trust_tier",
	"max_tool_rounds", "max_tokens", "role", "version", "examples", "streams_output",
}

// Required are the keys a profile must set.
var Required = []string{"name", "description", "capabilities", "skills", "tools", "trust_tier", "role"}

// Bounds of the optional integer fields (inclusive).
var (
	MaxToolRoundsBounds = [2]int64{1, 64}
	MaxTokensBounds     = [2]int64{256, 131072}
)

// Example-task bounds (ADR-0139 C2: "3-5 example tasks"). Optional; when set,
// 1..MaxExamples unique non-blank strings of at most MaxExampleRunes characters.
const (
	MaxExamples     = 5
	MaxExampleRunes = 300
)

// DefaultVersion is a profile's version when it sets none.
const DefaultVersion = "1.0.0"

var (
	reName        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	reCapability  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	reSkillName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	reVersion     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	reGeneratorID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	reMCPGlob     = regexp.MustCompile(`^mcp:([a-z0-9][a-z0-9._-]*)/([A-Za-z0-9_.*?-]+)$`)
	reNativeGlob  = regexp.MustCompile(`^[a-z][a-z0-9_.*?-]*$`)
)

// Profile is one validated profile.
type Profile struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Role          string   `json:"role"`
	Capabilities  []string `json:"capabilities"`
	Skills        []string `json:"skills"`
	Tools         []string `json:"tools"`
	TrustTier     string   `json:"trust_tier"`
	Model         string   `json:"model"`
	MaxToolRounds *int64   `json:"max_tool_rounds"`
	MaxTokens     *int64   `json:"max_tokens"`
	Version       string   `json:"version"`
	// Examples are example tasks the agent is for; the A2A card publishes them
	// on its skills (internal/agentcard). nil when the profile sets none.
	Examples []string `json:"examples"`
	// StreamsOutput declares that the agent pushes its own live step output
	// (agent-plane EmitStepOutput), so the kernel forwards no gateway deltas for its
	// steps (ADR-0139 amendment 2026-11-06). Optional; `true` or `false`.
	StreamsOutput bool `json:"streams_output"`
	// Warnings are non-fatal findings (a capability new to the fleet vocabulary).
	Warnings []string `json:"-"`
}

// Allowlist is the profile's tool allowlist as a domain.ToolAllowlist.
func (p Profile) Allowlist() domain.ToolAllowlist { return domain.ToolAllowlist(p.Tools) }

// Options controls validation.
type Options struct {
	// ExpectedName, when set, must equal the profile's name (the file stem).
	ExpectedName string
	// SkillTools maps a skill name to the tools its SKILL.md grants. nil skips the
	// two skill checks (every listed skill exists; every tool a listed skill grants
	// is inside the allowlist), as the SDK does with skills_dir=None.
	SkillTools map[string][]string
}

// Parse parses and validates one profile's text. The rules are the SDK's
// parse_profile, check for check.
func Parse(text string, opt Options) (Profile, error) {
	data, _, err := parseSubset(strings.ReplaceAll(text, "\r\n", "\n"))
	if err != nil {
		return Profile{}, fmt.Errorf("%v (profiles use the strict YAML subset of prompts/SPEC.md §2.2)", err)
	}
	var unknown []string
	for k := range data {
		if !contains(Fields, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Profile{}, fmt.Errorf("unknown field(s) %v; allowed: %v", unknown, sorted(Fields))
	}
	var missing []string
	for _, k := range Required {
		if isEmpty(data[k]) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Profile{}, fmt.Errorf("missing required field(s) %v", missing)
	}

	var p Profile
	if p.Name, err = strField(data, "name"); err != nil {
		return Profile{}, err
	}
	if !reName.MatchString(p.Name) {
		return Profile{}, fmt.Errorf("name %q must match %s", p.Name, reName)
	}
	if opt.ExpectedName != "" && p.Name != opt.ExpectedName {
		return Profile{}, fmt.Errorf("name %q must equal the file name %q", p.Name, opt.ExpectedName)
	}
	if p.Description, err = strField(data, "description"); err != nil {
		return Profile{}, err
	}
	// The kernel reads an AGENT_DESCRIPTION with a regex that stops at a quote;
	// profiles keep routing text quote-free so every copy carries the same string.
	if strings.ContainsAny(p.Description, `"'`) {
		return Profile{}, fmt.Errorf("description must not contain quote characters")
	}
	if p.Role, err = strField(data, "role"); err != nil {
		return Profile{}, err
	}

	caps, err := listField(data, "capabilities")
	if err != nil {
		return Profile{}, err
	}
	for _, c := range caps {
		s, ok := c.(string)
		if !ok || !reCapability.MatchString(s) {
			return Profile{}, fmt.Errorf("capability %s must be lower_snake_case (the fleet's declared spelling)", pyRepr(c))
		}
		if contains(ReservedCapabilities, s) {
			return Profile{}, fmt.Errorf("capability %q is reserved for the chat front desk", s)
		}
		if !contains(CapabilityVocabulary, s) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("capability %q is new to the fleet vocabulary; it routes only when a planner step requires it", s))
		}
		p.Capabilities = append(p.Capabilities, s)
	}

	skills, err := listField(data, "skills")
	if err != nil {
		return Profile{}, err
	}
	for _, sk := range skills {
		s, ok := sk.(string)
		if !ok || !reSkillName.MatchString(s) {
			return Profile{}, fmt.Errorf("skill name %s is malformed", pyRepr(sk))
		}
		p.Skills = append(p.Skills, s)
	}

	tools, err := listField(data, "tools")
	if err != nil {
		return Profile{}, err
	}
	for _, t := range tools {
		g, err := CheckGlob(t)
		if err != nil {
			return Profile{}, err
		}
		p.Tools = append(p.Tools, g)
	}

	if p.TrustTier, err = strField(data, "trust_tier"); err != nil {
		return Profile{}, err
	}
	if !contains(TrustTiers, p.TrustTier) {
		return Profile{}, fmt.Errorf("trust_tier %q must be one of %v", p.TrustTier, TrustTiers)
	}

	if !isNullOrBlank(data["model"]) {
		if p.Model, err = strField(data, "model"); err != nil {
			return Profile{}, err
		}
		if role, isRole := strings.CutPrefix(p.Model, "role:"); isRole {
			if !contains(ModelRoles, role) {
				return Profile{}, fmt.Errorf("model %q: role must be one of %v", p.Model, ModelRoles)
			}
		} else if !reGeneratorID.MatchString(p.Model) {
			return Profile{}, fmt.Errorf("model %q must be a generator id or role:<role>", p.Model)
		}
	}

	p.Version = DefaultVersion
	if !isNullOrBlank(data["version"]) {
		if p.Version, err = strField(data, "version"); err != nil {
			return Profile{}, err
		}
		if !reVersion.MatchString(p.Version) {
			return Profile{}, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", p.Version)
		}
	}

	if !isEmpty(data["examples"]) {
		ex, err := listField(data, "examples")
		if err != nil {
			return Profile{}, err
		}
		if len(ex) > MaxExamples {
			return Profile{}, fmt.Errorf("%q has %d entries; at most %d", "examples", len(ex), MaxExamples)
		}
		for _, e := range ex {
			s, ok := e.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return Profile{}, fmt.Errorf("example %s must be a non-empty string", pyRepr(e))
			}
			s = strings.TrimSpace(s)
			if utf8.RuneCountInString(s) > MaxExampleRunes {
				return Profile{}, fmt.Errorf("example %q is longer than %d characters", s, MaxExampleRunes)
			}
			p.Examples = append(p.Examples, s)
		}
	}

	if p.StreamsOutput, err = boolField(data, "streams_output"); err != nil {
		return Profile{}, err
	}

	if p.MaxToolRounds, err = intField(data, "max_tool_rounds", MaxToolRoundsBounds); err != nil {
		return Profile{}, err
	}
	if p.MaxTokens, err = intField(data, "max_tokens", MaxTokensBounds); err != nil {
		return Profile{}, err
	}

	if opt.SkillTools != nil {
		var absent []string
		for _, s := range p.Skills {
			if _, ok := opt.SkillTools[s]; !ok {
				absent = append(absent, s)
			}
		}
		if len(absent) > 0 {
			return Profile{}, fmt.Errorf("skill(s) %v not found in the kernel's skills", absent)
		}
		allow := p.Allowlist()
		for _, s := range p.Skills {
			var outside []string
			for _, t := range opt.SkillTools[s] {
				if !allow.Allows(t) {
					outside = append(outside, t)
				}
			}
			if len(outside) > 0 {
				return Profile{}, fmt.Errorf("skill %q grants %v, which the profile's tools do not allow; "+
					"add them to tools or drop the skill (the kernel would clip them and the skill's "+
					"procedure would fail at that step)", s, outside)
			}
		}
	}
	return p, nil
}

// CheckGlob validates one tool allowlist pattern (the SDK's _check_glob).
func CheckGlob(v any) (string, error) {
	pattern, ok := v.(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("tool pattern %s must be a non-empty string", pyRepr(v))
	}
	if pattern != strings.TrimSpace(pattern) || strings.ContainsFunc(pattern, isSpace) {
		return "", fmt.Errorf("tool pattern %q contains whitespace", pattern)
	}
	if strings.ContainsAny(pattern, "[]") || strings.Contains(pattern, "**") {
		return "", fmt.Errorf("tool pattern %q: only '*' and '?' are supported (no [...] classes, no **)", pattern)
	}
	if strings.HasPrefix(pattern, "mcp:") {
		if !reMCPGlob.MatchString(pattern) {
			return "", fmt.Errorf("tool pattern %q must be mcp:<server id>/<tool glob> with a literal "+
				"server id (no wildcard before the '/')", pattern)
		}
		return pattern, nil
	}
	if strings.ContainsAny(pattern, ":/") {
		return "", fmt.Errorf("tool pattern %q: unknown namespace (use mcp:<server>/<tool> or a native tool name)", pattern)
	}
	if !reNativeGlob.MatchString(pattern) || strings.Trim(pattern, "*?") == "" {
		return "", fmt.Errorf("tool pattern %q is not a native tool name; a bare wildcard would allow every tool", pattern)
	}
	return pattern, nil
}

// Loaded is one profile file's outcome.
type Loaded struct {
	Path    string
	Profile Profile
	Text    []byte // the file's bytes (for the agent's source hash)
	Err     error  // non-nil ⇒ the profile is invalid and must not be registered
}

// LoadDir reads every <dir>/*.yaml profile, sorted by file name. A missing dir
// yields nothing. Each file is validated on its own: one invalid profile never
// hides the others (the caller logs it loudly and skips it).
func LoadDir(dir string, skillTools map[string][]string) ([]Loaded, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]Loaded, 0, len(paths))
	for _, path := range paths {
		l := Loaded{Path: path}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			l.Err = fmt.Errorf("profile %s unreadable: %w", path, rerr)
			out = append(out, l)
			continue
		}
		l.Text = b
		stem := strings.TrimSuffix(filepath.Base(path), ".yaml")
		p, perr := Parse(string(b), Options{ExpectedName: stem, SkillTools: skillTools})
		if perr != nil {
			l.Err = fmt.Errorf("%s: %w", path, perr)
		}
		l.Profile = p
		out = append(out, l)
	}
	return out, nil
}

func strField(data map[string]any, key string) (string, error) {
	s, ok := data[key].(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%q must be a non-empty string", key)
	}
	return strings.TrimSpace(s), nil
}

func listField(data map[string]any, key string) ([]any, error) {
	l, ok := data[key].([]any)
	if !ok || len(l) == 0 {
		return nil, fmt.Errorf("%q must be a non-empty list", key)
	}
	seen := map[string]bool{}
	for _, v := range l {
		seen[pyStr(v)] = true
	}
	if len(seen) != len(l) {
		return nil, fmt.Errorf("%q has duplicate entries", key)
	}
	return l, nil
}

func intField(data map[string]any, key string, bounds [2]int64) (*int64, error) {
	v, present := data[key]
	if !present {
		return nil, nil
	}
	if _, isNull := v.(null); isNull {
		return nil, nil
	}
	i, ok := v.(int64)
	if !ok || i < bounds[0] || i > bounds[1] {
		return nil, fmt.Errorf("%q must be an integer in [%d, %d]", key, bounds[0], bounds[1])
	}
	return &i, nil
}

// isEmpty mirrors Python's `data.get(k) in (None, "", [])`.
// boolField reads an optional boolean. The frontmatter subset has no boolean
// scalar (an unquoted true is the string "true", as in the SDK's _scalar), so the
// field accepts exactly "true" or "false"; absent, null or blank is false.
func boolField(data map[string]any, key string) (bool, error) {
	v, ok := data[key]
	if !ok || isNullOrBlank(v) {
		return false, nil
	}
	if s, isStr := v.(string); isStr {
		switch strings.TrimSpace(s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("%q must be true or false (got %s)", key, pyRepr(v))
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil, null:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

// isNullOrBlank mirrors Python's `data.get(k) in (None, "")`.
func isNullOrBlank(v any) bool {
	switch x := v.(type) {
	case nil, null:
		return true
	case string:
		return x == ""
	}
	return false
}

func isSpace(r rune) bool {
	return strings.TrimSpace(string(r)) == ""
}

func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return pyStr(v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sorted(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}
