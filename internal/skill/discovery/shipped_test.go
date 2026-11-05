package discovery

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// shippedSkillsDir is cambrian-core/skills, the directory the kernel reads at boot
// when it runs from cambrian-core (app.go: skilldiscovery.LoadRegistry("skills", ...)).
func shippedSkillsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	// internal/skill/discovery -> repo root
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "skills")
}

// ADR-0139 D13 / ADR-0046: the shipped system skill is discovered from the real
// skills/ directory with its full body intact — not a head, not the frontmatter —
// and its grants and Tier-1/Tier-2 disclosure are what the kernel will serve.
func TestShippedSkills_FileReportLoadsWithFullBody(t *testing.T) {
	dir := shippedSkillsDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "file-report", "SKILL.md"))
	if err != nil {
		t.Fatalf("shipped skill missing: %v", err)
	}
	reg := domain.NewInMemorySkillRegistry()
	skills, err := LoadRegistry(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) == 0 {
		t.Fatal("no shipped skills discovered")
	}
	sk, ok := reg.Get("file-report")
	if !ok {
		t.Fatal("file-report not registered")
	}

	// Full body: everything after the closing frontmatter fence, byte for byte
	// (modulo the surrounding whitespace ParseSkillMD trims).
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	fence := strings.Index(s[3:], "\n---\n") + 3
	wantBody := strings.TrimSpace(s[fence+len("\n---\n"):])
	if sk.Instructions != wantBody {
		t.Fatalf("body not intact: got %d chars, want %d", len(sk.Instructions), len(wantBody))
	}
	if len(sk.Instructions) <= 1500 {
		t.Errorf("body is %d chars; the skill-adherence gate needs a >1500-char procedure", len(sk.Instructions))
	}
	for _, marker := range []string{"## 1. Pin down the request", "## 5. Verify every claim", "number of source files used."} {
		if !strings.Contains(sk.Instructions, marker) {
			t.Errorf("body lacks %q", marker)
		}
	}
	if strings.Contains(sk.Instructions, "tools:") || strings.Contains(sk.Instructions, "name: file-report") {
		t.Error("frontmatter leaked into the instructions")
	}
	if len(sk.ToolGrants) == 0 || !contains(sk.ToolGrants, "mcp:filesystem/write_file") {
		t.Errorf("grants = %v", sk.ToolGrants)
	}
	if len(sk.ScopeTags) != 0 {
		t.Errorf("file-report is general-purpose; scope = %v", sk.ScopeTags)
	}

	// Tier-1 menu line: the first sentence, uncut (it fits the 120-char cap).
	desc, instr, grants := domain.SkillDisclosure(sk, false)
	if strings.HasSuffix(desc, "…") || !strings.HasPrefix(desc, "Produce a structured written report") {
		t.Errorf("Tier-1 description = %q", desc)
	}
	if instr != "" || grants != nil {
		t.Error("Tier-1 must not carry the body or grants")
	}
	// Tier-2 (use_skill): the full body and the grants.
	_, instr, grants = domain.SkillDisclosure(sk, true)
	if instr != wantBody || len(grants) != len(sk.ToolGrants) {
		t.Error("Tier-2 must carry the full body and every grant")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var updateSummaryCases = flag.Bool("update-summary-cases", false,
	"rewrite testdata/summary_cases.json from the Go deriver")

// summaryCase is one row of the cross-language fixture. The SDK's derive_summary
// (sdk/cambrian_agent_sdk/react.py) is held to these rows by
// sdk/tests/test_skill_summary_parity.py, so an agent-local skill's menu line is
// derived exactly as the kernel derives a system skill's.
type summaryCase struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Want        string `json:"want"`
}

var summaryInputs = []summaryCase{
	{Name: "plain", Description: "Deploy the application to production."},
	{Name: "two-sentences", Description: "Write a report. Then verify it."},
	{Name: "first-line", Description: "Line one\nline two. more"},
	{Name: "crlf", Description: "Line one\r\nline two"},
	{Name: "markup", Description: "## **Heading** text here"},
	{Name: "spaces", Description: "  lots   of\tinner   space  "},
	{Name: "mcp:server/fallback_name-here", Description: ""},
	{Name: "only-markup", Description: "### "},
	{Name: "long", Description: strings.Repeat("procedure ", 20) + "end"},
	{Name: "long-no-space", Description: strings.Repeat("x", 130)},
	{Name: "unicode", Description: strings.Repeat("ünïcödé ", 20)},
	{Name: "period-no-space", Description: "v1.2 is the version.Next"},
}

// TestSummaryCasesFixture pins testdata/summary_cases.json to the Go deriver
// (domain.SkillDisclosure Tier-1, which is deriveSummary). Regenerate with
// go test ./internal/skill/discovery -run TestSummaryCasesFixture -update-summary-cases
func TestSummaryCasesFixture(t *testing.T) {
	cases := make([]summaryCase, len(summaryInputs))
	for i, c := range summaryInputs {
		c.Want, _, _ = domain.SkillDisclosure(domain.Skill{Name: c.Name, Description: c.Description}, false)
		cases[i] = c
	}
	path := filepath.Join("testdata", "summary_cases.json")
	got, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateSummaryCases {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (regenerate with -update-summary-cases): %v", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != string(got) {
		t.Fatal("testdata/summary_cases.json is stale; regenerate with -update-summary-cases")
	}
}
