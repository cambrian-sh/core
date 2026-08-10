package awareness

import (
	"runtime"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// TestBuildHostBlock_CarriesOnlyTheOS pins the division of labour decided on
// 2026-08-08. The planner cannot see the host it plans for, so it is told the OS
// (which is a property of the deployment) and NOT the home/desktop/cwd (which are
// properties of one account, and which the executing agent reads off the real
// machine via the SDK's host_facts).
//
// Two concrete failure modes this forbids:
//   - a plan step containing an absolute "C:\Users\<someone>\Desktop\..." is wrong
//     the moment it is replayed as a <PlanLTM> precedent under another account, and
//     it writes a username into memory;
//   - the planner guessing a home directory it was never shown.
func TestBuildHostBlock_CarriesOnlyTheOS(t *testing.T) {
	block := buildHostBlock("windows")

	if !strings.Contains(block, "os: windows") {
		t.Fatalf("host block must state the OS, got %q", block)
	}
	// Assert the premise, so this cannot pass vacuously on an empty block.
	if !strings.Contains(block, "<host>") || !strings.Contains(block, "</host>") {
		t.Fatalf("host block must be a delimited section, got %q", block)
	}
	for _, forbidden := range []string{"home", "desktop", "cwd", "Users"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("host block leaked %q to the planner — paths are the executor's job: %q",
				forbidden, block)
		}
	}
}

func TestBuildHostBlock_EmptyGOOSSaysNothing(t *testing.T) {
	// Better to omit the section than to announce an unknown platform.
	if got := buildHostBlock(""); got != "" {
		t.Errorf("buildHostBlock(\"\") = %q, want empty", got)
	}
}

// TestHostGOOS_IsLiveUnlessPinned covers the owner's requirement that the OS is
// discovered dynamically rather than baked in.
func TestHostGOOS_IsLiveUnlessPinned(t *testing.T) {
	p := &Planner{}
	if got := p.hostGOOS(); got != runtime.GOOS {
		t.Errorf("bare Planner reported %q, want the live host %q", got, runtime.GOOS)
	}

	pinned := &Planner{goos: "plan9"}
	if got := pinned.hostGOOS(); got != "plan9" {
		t.Errorf("pinned Planner reported %q, want %q", got, "plan9")
	}
}

// TestPlannerPromptHash_ExcludesTheHost is the regression that matters most for a
// fleet: plannerPromptHash gates plan-cache reuse through
// ExecutionPlan.PlannerPromptVersion, so folding a per-host value into the hashed
// static text would give every host a different hash and silently invalidate
// every cached plan. The host block must therefore be a DYNAMIC constraint.
func TestPlannerPromptHash_ExcludesTheHost(t *testing.T) {
	// The rule text is ALLOWED to reference the section by name ("the <host>
	// section tells you the OS") — that is static guidance and belongs in the hash.
	// What must never be baked in is the BLOCK, because it carries a per-host value.
	block := buildHostBlock(runtime.GOOS)
	if block == "" {
		t.Fatal("premise failed: buildHostBlock produced nothing, so the check below proves nothing")
	}
	for name, static := range map[string]string{
		"plannerStaticText":    plannerStaticText,
		"plannerStaticTextCap": plannerStaticTextCap,
	} {
		if strings.Contains(static, block) {
			t.Errorf("%s contains the rendered <host> block; a per-host value in the hashed text gives every host a different plannerPromptHash and silently invalidates cached plans", name)
		}
		if strings.Contains(static, "os: "+runtime.GOOS) {
			t.Errorf("%s hardcodes the OS fact %q", name, runtime.GOOS)
		}
	}
	// The hash is a pure function of that text, so equal text ⇒ equal hash on
	// every machine. Asserted directly rather than trusting the string check.
	if domain.PromptHashOf(plannerStaticText) != plannerPromptHash {
		t.Error("plannerPromptHash is not the hash of plannerStaticText")
	}
	if plannerPromptHash == plannerPromptHashCap {
		t.Error("the two arms must hash differently")
	}
}

// TestPlannerRules_ForbidConstructingPaths pins the static half of the rule. The
// bug being prevented: when the Scout was retired on 2026-08-07 the ONLY
// instruction against emitting "~/Desktop" disappeared with the <environment>
// block it lived inside — on Windows, where no shell expands "~".
func TestPlannerRules_ForbidConstructingPaths(t *testing.T) {
	if !strings.Contains(plannerDecisionRules, "~") {
		t.Error("the decision rules must mention \"~\" in order to forbid it")
	}
	lowered := strings.ToLower(plannerDecisionRules)
	for _, want := range []string{"executor's job", "do not construct absolute paths"} {
		if !strings.Contains(lowered, strings.ToLower(want)) {
			t.Errorf("the decision rules must say %q", want)
		}
	}
}

// TestPlannerRules_DropDeadDiscoverySections guards the other half: the Scout is
// gone, so instructing the model about sections nothing can produce is dead
// prompt text shipped on every planning call.
func TestPlannerRules_DropDeadDiscoverySections(t *testing.T) {
	for _, dead := range []string{"<DiscoveryLTM>", "<environment>"} {
		if strings.Contains(plannerLTMRules, dead) {
			t.Errorf("plannerLTMRules still instructs on %s, which nothing produces since the Scout was retired", dead)
		}
	}
	// Premise: the surviving LTM sections are still described, so the test above
	// is not passing merely because the rules block was emptied.
	for _, live := range []string{"<FactLTM>", "<PlanLTM>", "<NegativeLTM>"} {
		if !strings.Contains(plannerLTMRules, live) {
			t.Errorf("plannerLTMRules lost %s", live)
		}
	}
}
