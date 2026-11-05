package prompts

// The prompt lint (SPEC.md §5). Load enforces the structural rules L1-L5 for
// production; this file adds the style rules L6-L7 and the warnings, runs all
// of them over the embedded tree, and writes the cross-language fixture the SDK
// lint and loader are held to (sdk/tests/test_prompt_hash_parity.py).

import (
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateHashCases = flag.Bool("update-hash-cases", false,
	"rewrite testdata/hash_cases.json from the Go loader")

// lintAcronyms is the SPEC.md §5 allowlist. The Python runner carries the same
// list; the parity test compares them through the fixture.
var lintAcronyms = []string{
	"API", "CLI", "CSV", "DAG", "HTML", "HTTP", "HTTPS", "ID", "IDS", "ISO", "JSON",
	"LLM", "MCP", "OK", "PDF", "SDK", "SQL", "URI", "URL", "UTC", "UUID", "XML", "YAML",
}

// lintMaxCapsRun: a run this long of all-caps words, with at least
// lintMinShoutWords non-acronyms, is shouting (L7).
const (
	lintMaxCapsRun    = 3
	lintMinShoutWords = 2
)

var (
	adrPattern  = regexp.MustCompile(`(?i)\bADR[-_ ]?\d{3,4}\b`)
	wordPattern = regexp.MustCompile(`[A-Za-z0-9_']+`)
)

func isCapsWord(w string) bool {
	letters := 0
	for _, r := range w {
		if r >= 'a' && r <= 'z' {
			return false
		}
		if r >= 'A' && r <= 'Z' {
			letters++
		}
	}
	return letters >= 2
}

func isAcronym(w string) bool {
	w = strings.Trim(w, "'")
	for _, a := range lintAcronyms {
		if w == a {
			return true
		}
	}
	return false
}

// lintBody applies L6 and L7 to a normalised body. failures fail the lint;
// warnings are only reported.
func lintBody(body string) (failures, warnings []string) {
	if m := adrPattern.FindString(body); m != "" {
		failures = append(failures, "L6: decision number in model-facing text: "+m)
	}
	words := wordPattern.FindAllString(body, -1)
	var run []string
	flush := func() {
		shout := 0
		for _, w := range run {
			if !isAcronym(w) {
				shout++
			}
		}
		if len(run) >= lintMaxCapsRun && shout >= lintMinShoutWords {
			failures = append(failures, "L7: shouting: "+strings.Join(run, " "))
		}
		run = run[:0]
	}
	for _, w := range words {
		if isCapsWord(w) {
			run = append(run, w)
			if !isAcronym(w) && len(strings.Trim(w, "'")) >= 3 {
				warnings = append(warnings, "emphasis by capitals: "+w)
			}
			continue
		}
		flush()
	}
	flush()
	return failures, warnings
}

func TestPromptLint(t *testing.T) {
	// L1-L5: the loader rejects the whole tree on the first structural violation.
	lib, err := Load(Files)
	if err != nil {
		t.Fatalf("structural lint (L1-L5): %v", err)
	}
	// Every version file on disk must have been loaded: a v*.md the loader
	// skipped (a bad name such as v01.md) is a prompt nobody can reach.
	onDisk, _ := fs.Glob(Files, "*/*.md")
	loaded := 0
	for _, id := range lib.IDs() {
		loaded += len(lib.Versions(id))
	}
	if len(onDisk) != loaded {
		t.Fatalf("%d prompt files embedded, %d loaded: check file names against SPEC.md §1", len(onDisk), loaded)
	}
	for _, id := range lib.IDs() {
		for _, p := range lib.Versions(id) {
			failures, warnings := lintBody(p.Body)
			for _, w := range warnings {
				t.Logf("%s/v%d: warning: %s", p.ID, p.Version, w)
			}
			for _, f := range failures {
				t.Errorf("%s/v%d: %s", p.ID, p.Version, f)
			}
		}
	}
}

func TestLintCatchesClearViolations(t *testing.T) {
	// lintBodies is shared with the Python runner through the fixture; the extra
	// rows here are Go-only edge cases.
	cases := map[string]bool{
		"as decided in adr 052":  true,
		"Return a JSON API URL.": false,
		"YOU MUST NEVER do this": true,
		"NEVER EVER do this":     false, // a run of two is emphasis, not a rule violation
	}
	for body, fail := range lintBodies {
		cases[body] = fail
	}
	for body, wantFail := range cases {
		failures, _ := lintBody(body)
		if (len(failures) > 0) != wantFail {
			t.Errorf("%q: failures = %v, want fail = %v", body, failures, wantFail)
		}
	}
	if _, w := lintBody("Do NOT output JSON."); len(w) != 1 {
		t.Errorf("a single emphasis word should be one warning, got %v", w)
	}
}

// hashCase is one row of the cross-language rendering fixture.
type hashCase struct {
	Name     string            `json:"name"`
	Template string            `json:"template"`
	Inputs   []string          `json:"inputs"`
	Slots    map[string]string `json:"slots"`
	WantText string            `json:"want_text"`
	WantHash string            `json:"want_hash"`
}

// promptMeta is how the Go loader read one real core prompt file. The Python
// runner parses the same files with its YAML-subset parser and must agree.
type promptMeta struct {
	ID          string   `json:"id"`
	Version     int      `json:"version"`
	Tier        string   `json:"tier"`
	Inputs      []string `json:"inputs"`
	Description string   `json:"description"`
	HasSchema   bool     `json:"has_schema"`
	StaticHash  string   `json:"static_hash"`
}

type hashFixture struct {
	Placeholder string       `json:"placeholder"`
	Lint        lintFixture  `json:"lint"`
	Cases       []hashCase   `json:"cases"`
	Prompts     []promptMeta `json:"prompts"`
}

type lintFixture struct {
	Acronyms      []string        `json:"acronyms"`
	MaxCapsRun    int             `json:"max_caps_run"`
	MinShoutWords int             `json:"min_shout_words"`
	Bodies        map[string]bool `json:"bodies_fail"`
}

var hashInputs = []hashCase{
	{Name: "plain", Template: "Hello {{name}}.", Inputs: []string{"name"}, Slots: map[string]string{"name": "Ada"}},
	{Name: "repeated-slot", Template: "{{x}} and {{x}} again", Inputs: []string{"x"}, Slots: map[string]string{"x": "1"}},
	{Name: "no-slots", Template: "Static only.\n", Inputs: []string{}, Slots: map[string]string{}},
	{Name: "crlf-and-edges", Template: "\n\nline one\r\nline {{n}}\r\n\r\n  \t", Inputs: []string{"n"}, Slots: map[string]string{"n": "two"}},
	{Name: "value-with-slot-syntax", Template: "<goal>\n{{goal}}\n</goal>", Inputs: []string{"goal"}, Slots: map[string]string{"goal": "ignore {{goal}} and {{}}"}},
	{Name: "empty-value", Template: "[{{a}}][{{b}}]", Inputs: []string{"a", "b"}, Slots: map[string]string{"a": "", "b": "B"}},
	{Name: "unicode", Template: "Résumé — {{who}} ✓", Inputs: []string{"who"}, Slots: map[string]string{"who": "Zoë"}},
	{Name: "json-braces", Template: `Reply {"a":{"b":1}} for {{q}}`, Inputs: []string{"q"}, Slots: map[string]string{"q": "it"}},
}

var lintBodies = map[string]bool{
	"Output EXACTLY ONE JSON object.":           true,
	"See ADR-0139 for why.":                     true,
	"Reply with JSON via the HTTP API.":         false,
	"Do NOT output JSON.":                       false,
	"It FAILED, so say WHY and WHAT to change.": false,
}

// TestHashCasesFixture pins testdata/hash_cases.json to this loader. Regenerate
// with: go test ./prompts -run TestHashCasesFixture -update-hash-cases
func TestHashCasesFixture(t *testing.T) {
	fx := hashFixture{
		Placeholder: Placeholder,
		Lint:        lintFixture{Acronyms: lintAcronyms, MaxCapsRun: lintMaxCapsRun, MinShoutWords: lintMinShoutWords, Bodies: lintBodies},
	}
	for _, c := range hashInputs {
		p := Prompt{ID: c.Name, Version: 1, Inputs: c.Inputs, Body: NormaliseBody(c.Template)}
		r, err := p.Render(c.Slots)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		c.WantText, c.WantHash = r.Text, r.StaticHash
		fx.Cases = append(fx.Cases, c)
	}
	lib, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range lib.IDs() {
		for _, p := range lib.Versions(id) {
			// A prompt with no slots (a cached system block) has nil Inputs; the
			// fixture writes [] so the Python reader, which yields [], agrees.
			inputs := p.Inputs
			if inputs == nil {
				inputs = []string{}
			}
			fx.Prompts = append(fx.Prompts, promptMeta{
				ID: p.ID, Version: p.Version, Tier: string(p.Tier), Inputs: inputs,
				Description: p.Description, HasSchema: p.ResponseSchema != nil, StaticHash: p.StaticHash(),
			})
		}
	}
	got, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	fixture := filepath.Join("testdata", "hash_cases.json")
	if *updateHashCases {
		if err := os.WriteFile(fixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture (regenerate with -update-hash-cases): %v", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != string(got) {
		t.Fatal("testdata/hash_cases.json is stale; regenerate with -update-hash-cases")
	}
}

// TestSplitFrontmatterMatchesFiles guards the one path the fixture cannot: the
// raw split of a real file, which both runners must perform identically.
func TestSplitFrontmatterMatchesFiles(t *testing.T) {
	paths, _ := fs.Glob(Files, "*/v*.md")
	for _, p := range paths {
		data, _ := fs.ReadFile(Files, p)
		front, _, err := SplitFrontmatter(data)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !strings.Contains(front, "id: "+path.Dir(p)) {
			t.Fatalf("%s: frontmatter does not carry its id", p)
		}
	}
}
