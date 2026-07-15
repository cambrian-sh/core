package domain

import (
	"context"
	"strings"
	"testing"
)

// ADR-0051 D9: an empty/nil report renders nothing — the degrade-to-one-shot signal.
func TestDiscoveryReferencedByPlan(t *testing.T) {
	report := &DiscoveryReport{
		Entities: []DiscoveredEntity{
			{Kind: "file", ID: "internal/memory/chunker.go", Summary: "10 funcs"},
		},
		Unobserved:  []string{"service:vectordb"},
		Environment: &EnvFacts{OS: "windows", Cwd: `C:\proj`},
	}
	plan := func(qs ...string) *ExecutionPlan {
		p := &ExecutionPlan{}
		for _, q := range qs {
			p.Steps = append(p.Steps, Step{Query: q})
		}
		return p
	}

	// A plan that names a discovered entity ⇒ referenced.
	if !DiscoveryReferencedByPlan(report, plan("Read internal/memory/chunker.go and summarise")) {
		t.Error("plan naming a discovered entity id must count as referenced")
	}
	// A plan that names an unobserved entity (told to scan it) ⇒ referenced.
	if !DiscoveryReferencedByPlan(report, plan("Scan service:vectordb before planning")) {
		t.Error("plan naming an unobserved entity must count as referenced")
	}
	// A plan that only echoes the cwd path ⇒ NOT referenced (env is boilerplate).
	if DiscoveryReferencedByPlan(report, plan(`Create a report in C:\proj`)) {
		t.Error("environment-path echo must NOT count as referenced (noise)")
	}
	// A plan referencing nothing discovered ⇒ not referenced.
	if DiscoveryReferencedByPlan(report, plan("Write a poem about the sea")) {
		t.Error("unrelated plan must not count as referenced")
	}
	// Env-only report (no concrete entity) ⇒ never referenced.
	envOnly := &DiscoveryReport{Environment: &EnvFacts{OS: "windows", Cwd: `C:\proj`}}
	if DiscoveryReferencedByPlan(envOnly, plan(`work in C:\proj`)) {
		t.Error("env-only report carries no entity to reference")
	}
	// Nil-safety.
	if DiscoveryReferencedByPlan(nil, plan("x")) || DiscoveryReferencedByPlan(report, nil) {
		t.Error("nil report/plan must be safe and false")
	}
}

func TestRenderDiscoveryBlock_EmptyIsDegradeSignal(t *testing.T) {
	if got := RenderDiscoveryBlock(nil); got != "" {
		t.Errorf("nil report must render empty; got %q", got)
	}
	if got := RenderDiscoveryBlock(&DiscoveryReport{}); got != "" {
		t.Errorf("empty report must render empty; got %q", got)
	}
	if !(&DiscoveryReport{}).IsEmpty() || (&DiscoveryReport{Entities: []DiscoveredEntity{{}}}).IsEmpty() {
		t.Error("IsEmpty must be true only when there are no entities, interpretation, or unobserved")
	}
}

// ADR-0051 D9: structured entities render as trusted attributes with the compact summary;
// the raw body is referenced by content_cid and never inlined.
func TestRenderDiscoveryBlock_StructuredAndCidReferenced(t *testing.T) {
	r := &DiscoveryReport{
		Entities: []DiscoveredEntity{
			{Kind: "dir", ID: "helicopter", Exists: true, Summary: "10 sections, 3 written, 7 missing", ContentCID: "cid-raw-listing"},
		},
		Interpretation: "one file per section; 7 remain",
		Unobserved:     []string{"api:example.com"},
	}
	got := RenderDiscoveryBlock(r)

	for _, want := range []string{
		"<DiscoveryLTM>", "</DiscoveryLTM>",
		`kind="dir"`, `id="helicopter"`, `exists="true"`,
		"10 sections, 3 written, 7 missing",
		`content_cid="cid-raw-listing"`,
		"<interpretation>one file per section; 7 remain</interpretation>",
		"<unobserved>api:example.com</unobserved>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered block missing %q\n---\n%s", want, got)
		}
	}
	// The raw listing content itself is NOT inlined — only its cid reference is present.
	if strings.Contains(got, "cid-raw-listing") && strings.Count(got, "cid-raw-listing") != 1 {
		t.Errorf("content_cid should appear once as a reference, not be expanded; got\n%s", got)
	}
}

// ADR-0051 env grounding: an env-only report grounds the Planner (non-empty) and renders
// host paths verbatim (Windows backslashes NOT doubled — the %q bug this guards against).
func TestRenderDiscoveryBlock_Environment(t *testing.T) {
	r := &DiscoveryReport{Environment: &EnvFacts{OS: "windows", Home: `C:\Users\x`, Desktop: `C:\Users\x\Desktop`, Cwd: `C:\proj`}}
	if r.IsEmpty() {
		t.Fatal("an env-only report grounds the planner and must not be empty")
	}
	got := RenderDiscoveryBlock(r)
	for _, want := range []string{`<environment`, `os="windows"`, `desktop="C:\Users\x\Desktop"`, `cwd="C:\proj"`} {
		if !strings.Contains(got, want) {
			t.Errorf("env block missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, `\\`) {
		t.Errorf("windows backslashes must not be doubled; got\n%s", got)
	}
}

// The Scout report rides ctx from Server.Execute into the Planner; an empty report reads
// back as "no discovery" so the Planner plans one-shot.
func TestDiscoveryContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := DiscoveryFromContext(ctx); ok {
		t.Error("bare context must report no discovery")
	}
	r := &DiscoveryReport{Entities: []DiscoveredEntity{{Kind: "file", ID: "a.md", Exists: true}}}
	got, ok := DiscoveryFromContext(WithDiscovery(ctx, r))
	if !ok || got != r {
		t.Errorf("attached report must round-trip; ok=%v got=%v", ok, got)
	}
	// An empty report attached to ctx still reads as "no discovery" (degrade).
	if _, ok := DiscoveryFromContext(WithDiscovery(ctx, &DiscoveryReport{})); ok {
		t.Error("an empty report must read back as no discovery (degrade-to-one-shot)")
	}
}
