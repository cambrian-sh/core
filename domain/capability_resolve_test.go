package domain

import (
	"errors"
	"strings"
	"testing"
)

func vocab() []string {
	return []string{"calculation", "research", "shell_execution", "analysis", GeneralistCapability}
}

// fleetSets mirrors vocab() as PER-AGENT declarations: one specialist per tag,
// each also a generalist. Eligibility is agent-wise, so the resolver needs these
// rather than the flat union.
func fleetSets() [][]string {
	return [][]string{
		{"calculation", GeneralistCapability},
		{"research", GeneralistCapability},
		{"shell_execution", GeneralistCapability},
		{"analysis", GeneralistCapability},
	}
}

func TestResolve_ExactMatch(t *testing.T) {
	r := ResolveCapabilities([]string{"calculation"}, vocab(), nil, fleetSets())
	if r.Tier != TierExact {
		t.Errorf("tier = %q, want %q", r.Tier, TierExact)
	}
	if len(r.Resolved) != 1 || r.Resolved[0] != "calculation" {
		t.Errorf("resolved = %v, want [calculation]", r.Resolved)
	}
	if len(r.Unmatched) != 0 {
		t.Errorf("unmatched = %v, want none", r.Unmatched)
	}
}

// Rung 1: normalization, so casing/separator variance is not a routing failure.
func TestResolve_NormalizationIsRungOne(t *testing.T) {
	r := ResolveCapabilities([]string{"Shell-Execution"}, vocab(), nil, fleetSets())
	if r.Tier != TierExact {
		t.Fatalf("tier = %q, want %q (normalization should match verbatim)", r.Tier, TierExact)
	}
	if r.Resolved[0] != "shell_execution" { // declared spelling in vocab()
		t.Errorf("resolved = %v, want [shell_execution]", r.Resolved)
	}
}

// Rung 2: an authored alias resolves a planner tag the fleet spells differently.
func TestResolve_AliasRung(t *testing.T) {
	aliases := map[string]string{"web_search": "research"}
	r := ResolveCapabilities([]string{"web-search"}, vocab(), aliases, fleetSets())
	if r.Tier != TierAlias {
		t.Fatalf("tier = %q, want %q", r.Tier, TierAlias)
	}
	if r.Resolved[0] != "research" {
		t.Errorf("resolved = %v, want [research]", r.Resolved)
	}
}

// An alias pointing at something the fleet does NOT declare is not a match.
func TestResolve_AliasToUnknownTargetDoesNotMatch(t *testing.T) {
	aliases := map[string]string{"web_search": "browser_automation"}
	r := ResolveCapabilities([]string{"web_search"}, vocab(), aliases, fleetSets())
	if r.Tier != TierGeneralist {
		t.Errorf("tier = %q, want %q (alias target is not in the vocabulary)", r.Tier, TierGeneralist)
	}
}

// Rung 3: an unknown requirement falls back to generalists rather than gating
// on a tag nobody declares (which would admit nobody).
func TestResolve_GeneralistRung(t *testing.T) {
	r := ResolveCapabilities([]string{"pdf_extract"}, vocab(), nil, fleetSets())
	if r.Tier != TierGeneralist {
		t.Fatalf("tier = %q, want %q", r.Tier, TierGeneralist)
	}
	if len(r.Resolved) != 1 || r.Resolved[0] != GeneralistCapability {
		t.Errorf("resolved = %v, want [%s]", r.Resolved, GeneralistCapability)
	}
	if len(r.Unmatched) != 1 || r.Unmatched[0] != "pdf_extract" {
		t.Errorf("unmatched = %v, want [pdf_extract]", r.Unmatched)
	}
	if !r.Satisfiable() {
		t.Error("generalist tier must be satisfiable")
	}
}

// Rung 4: nothing matches and there is no generalist ⇒ unsatisfiable, and the
// gate keeps the ORIGINAL requirements so it admits nobody.
func TestResolve_UnsatisfiableWithoutGeneralist(t *testing.T) {
	noGeneralist := []string{"calculation", "research"}
	r := ResolveCapabilities([]string{"pdf_extract"}, noGeneralist, nil, [][]string{{"calculation"}, {"research"}})
	if r.Tier != TierUnsatisfiable {
		t.Fatalf("tier = %q, want %q", r.Tier, TierUnsatisfiable)
	}
	if r.Satisfiable() {
		t.Error("unsatisfiable resolution must report Satisfiable() == false")
	}
}

// The whole point of ADR-0067's rejection of fuzzy matching: semantically
// opposite capabilities are embedding-close, so they must NEVER auto-match.
func TestResolve_NeverFuzzyMatchesOppositeCapabilities(t *testing.T) {
	fleet := []string{"file_read", GeneralistCapability}
	r := ResolveCapabilities([]string{"file_write"}, fleet, nil, [][]string{{"file_read"}, {GeneralistCapability}})
	for _, c := range r.Resolved {
		if c == "file_read" {
			t.Fatal("file_write must never resolve to file_read — that is a silent misroute")
		}
	}
	if r.Tier != TierGeneralist {
		t.Errorf("tier = %q, want %q", r.Tier, TierGeneralist)
	}
}

func TestResolve_MixedRequirementsOneUnknownFallsBack(t *testing.T) {
	r := ResolveCapabilities([]string{"calculation", "pdf_extract"}, vocab(), nil, fleetSets())
	if r.Tier != TierGeneralist {
		t.Errorf("tier = %q, want %q when any requirement is unmatched", r.Tier, TierGeneralist)
	}
}

func TestResolve_EmptyRequirementsIsANoOp(t *testing.T) {
	r := ResolveCapabilities(nil, vocab(), nil, fleetSets())
	if r.Tier != TierExact || len(r.Resolved) != 0 {
		t.Errorf("empty requirements should resolve to nothing, got tier=%q resolved=%v", r.Tier, r.Resolved)
	}
}

func TestBuildCapabilityVocabulary(t *testing.T) {
	ms := []*AgentManifest{
		{Capabilities: []string{"Calculation", "general_purpose"}},
		{Capabilities: []string{"calculation", "research"}},
		nil,
	}
	got := BuildCapabilityVocabulary(ms)
	// De-duped by NORMALIZED form ("Calculation" ≡ "calculation") but each entry
	// keeps the fleet's own spelling — the L1 gate compares verbatim unless
	// canonical_vocab is on.
	want := []string{"Calculation", "general_purpose", "research"}
	if len(got) != len(want) {
		t.Fatalf("vocabulary = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("vocabulary = %v, want %v (sorted, de-duped by normalized form)", got, want)
		}
	}
}

// A resolved capability must be usable by a VERBATIM L1 comparison, so it has to
// come back in the fleet's declared spelling — not the normalized one.
func TestResolve_ReturnsDeclaredSpellingNotNormalized(t *testing.T) {
	fleet := []string{"Shell_Execution", GeneralistCapability}
	r := ResolveCapabilities([]string{"shell-execution"}, fleet, nil, [][]string{{"Shell_Execution", GeneralistCapability}})
	if r.Tier != TierExact {
		t.Fatalf("tier = %q, want %q", r.Tier, TierExact)
	}
	if r.Resolved[0] != "Shell_Execution" {
		t.Errorf("resolved = %v, want the declared spelling [Shell_Execution]", r.Resolved)
	}
}

// REGRESSION (live orchestration probe, 2026-07-29): every required tag existed
// in the fleet vocabulary, so resolution reported TierExact — but the tags were
// spread across DIFFERENT agents and L1 needs ONE agent to hold them all. Result:
// every candidate filtered, empty slate, `no_candidate` on 2 of 3 tasks.
// Union membership is not eligibility.
func TestResolve_ConjunctionNoSingleAgentSatisfiesFallsBack(t *testing.T) {
	vocabulary := []string{"code_search", "file_read", GeneralistCapability}
	split := [][]string{
		{"code_search", GeneralistCapability}, // one agent has code_search
		{"file_read", GeneralistCapability},   // another has file_read
	}
	r := ResolveCapabilities([]string{"code_search", "file_read"}, vocabulary, nil, split)
	if r.Tier != TierGeneralist {
		t.Fatalf("tier = %q, want %q — no single agent holds both tags", r.Tier, TierGeneralist)
	}
	if len(r.Resolved) != 1 || r.Resolved[0] != GeneralistCapability {
		t.Errorf("resolved = %v, want [%s]", r.Resolved, GeneralistCapability)
	}
}

// The same conjunction IS exact when one agent declares the whole set.
func TestResolve_ConjunctionSatisfiedByOneAgentStaysExact(t *testing.T) {
	vocabulary := []string{"code_search", "file_read", GeneralistCapability}
	together := [][]string{{"code_search", "file_read", GeneralistCapability}}
	r := ResolveCapabilities([]string{"code_search", "file_read"}, vocabulary, nil, together)
	if r.Tier != TierExact {
		t.Fatalf("tier = %q, want %q — one agent declares both", r.Tier, TierExact)
	}
	if len(r.Resolved) != 2 {
		t.Errorf("resolved = %v, want both tags", r.Resolved)
	}
}

// An unsatisfiable conjunction with no generalist must still fail loudly.
func TestResolve_ConjunctionUnsatisfiableWithoutGeneralist(t *testing.T) {
	vocabulary := []string{"code_search", "file_read"}
	split := [][]string{{"code_search"}, {"file_read"}}
	r := ResolveCapabilities([]string{"code_search", "file_read"}, vocabulary, nil, split)
	if r.Tier != TierUnsatisfiable {
		t.Fatalf("tier = %q, want %q", r.Tier, TierUnsatisfiable)
	}
	if r.Satisfiable() {
		t.Error("must report Satisfiable() == false")
	}
}

// D5 requires the failure to NAME the gap and the live vocabulary.
func TestNoCapabilityMatchError_NamesGapAndVocabulary(t *testing.T) {
	err := &NoCapabilityMatchError{
		TaskID:     "task-3",
		Unmatched:  []string{"pdf_extract"},
		Vocabulary: []string{"calculation", "research"},
	}
	msg := err.Error()
	for _, want := range []string{"task-3", "pdf_extract", "calculation", "research"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}

	var target *NoCapabilityMatchError
	if !errors.As(error(err), &target) {
		t.Error("NoCapabilityMatchError must be recoverable with errors.As")
	}
}

func TestNoCapabilityMatchError_EmptyVocabularyReadsSensibly(t *testing.T) {
	err := &NoCapabilityMatchError{TaskID: "t", Unmatched: []string{"x"}}
	if strings.Contains(err.Error(), "[]") {
		t.Errorf("empty vocabulary should read as prose, got: %s", err.Error())
	}
}
