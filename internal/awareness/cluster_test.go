package awareness

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// TestPlannerPrompt_ContainsCapabilityClusters verifies the generated system
// prompt uses the CAPABILITY CLUSTERS block instead of the old AVAILABLE AGENTS flat list.
func TestPlannerPrompt_ContainsCapabilityClusters(t *testing.T) {
	provider := &agentProviderWithAgents{
		agents: []domain.AgentDefinition{
			{ID: "ocr-agent", Capabilities: []string{"vision"}},
		},
	}
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, provider, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "read the image")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := gen.capturedPrompts[0]

	if !strings.Contains(prompt, "CAPABILITY CLUSTERS") {
		t.Errorf("prompt must contain CAPABILITY CLUSTERS\nprompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "AVAILABLE AGENTS") {
		t.Errorf("prompt must NOT contain AVAILABLE AGENTS (replaced by CAPABILITY CLUSTERS)\nprompt:\n%s", prompt)
	}
}

// TestPlannerPrompt_ContainsGatekeeperInstruction verifies the clarifying
// Gatekeeper auction instruction is present in STRICT DECISION RULES.
func TestPlannerPrompt_ContainsRuntimeSelectionInstruction(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := gen.capturedPrompts[0]

	// The prompt must tell the planner the runtime resolves each step to a
	// concrete agent at execution time (so the planner drafts capability-level
	// descriptions), without committing to a specific selection mechanism — the
	// mechanism is flag-driven (auction or EFE), not baked into the prompt.
	if !strings.Contains(prompt, "resolves each step to a concrete agent at execution time") {
		t.Errorf("prompt missing runtime-selection clarifying instruction\nprompt:\n%s", prompt)
	}
}

// TestBuildCapabilityCluster_EmptySlice verifies an empty agent slice returns
// only the header line with no cluster entries.
func TestBuildCapabilityCluster_EmptySlice(t *testing.T) {
	out := buildCapabilityCluster([]domain.AgentDefinition{})

	if !strings.Contains(out, "CAPABILITY CLUSTERS") {
		t.Errorf("output missing header line\ngot: %q", out)
	}
	// No cluster entries — no "- " lines beyond the header
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "- ") {
			t.Errorf("expected no cluster entries for empty input, found: %q", line)
		}
	}
}

// TestBuildCapabilityCluster_AlphabeticalOrder verifies cluster headings appear
// in alphabetical order regardless of agent registration order.
func TestBuildCapabilityCluster_AlphabeticalOrder(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "z-agent", Capabilities: []string{"zygote-processing"}},
		{ID: "a-agent", Capabilities: []string{"audio-synthesis"}},
		{ID: "m-agent", Capabilities: []string{"media-encoding"}},
	}

	out := buildCapabilityCluster(agents)

	audioIdx := strings.Index(out, "audio-synthesis")
	mediaIdx := strings.Index(out, "media-encoding")
	zygoteIdx := strings.Index(out, "zygote-processing")

	if audioIdx == -1 || mediaIdx == -1 || zygoteIdx == -1 {
		t.Fatalf("missing cluster heading(s) in output:\n%s", out)
	}
	if !(audioIdx < mediaIdx && mediaIdx < zygoteIdx) {
		t.Errorf("clusters not in alphabetical order: audio=%d media=%d zygote=%d\nout: %s",
			audioIdx, mediaIdx, zygoteIdx, out)
	}
}

// TestBuildCapabilityCluster_MultiCapability verifies an agent with multiple
// Capabilities appears under each of its clusters.
func TestBuildCapabilityCluster_MultiCapability(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "swiss-agent", Capabilities: []string{"vision", "text"}},
	}

	out := buildCapabilityCluster(agents)

	if !strings.Contains(out, "vision:") {
		t.Errorf("output missing 'vision:' cluster\ngot: %s", out)
	}
	if !strings.Contains(out, "text:") {
		t.Errorf("output missing 'text:' cluster\ngot: %s", out)
	}
	// swiss-agent must appear twice — once under each cluster
	count := strings.Count(out, "swiss-agent")
	if count != 2 {
		t.Errorf("swiss-agent must appear 2 times (once per cluster), got %d\nout: %s", count, out)
	}
}

// TestBuildCapabilityCluster_TraitModelExcluded verifies TraitModel agents
// never appear in the cluster output, even when they have Capabilities set.
func TestBuildCapabilityCluster_TraitModelExcluded(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "gpt-4o", Trait: domain.TraitModel, Capabilities: []string{"reasoning"}},
		{ID: "ocr-agent", Capabilities: []string{"vision"}},
	}

	out := buildCapabilityCluster(agents)

	if strings.Contains(out, "gpt-4o") {
		t.Errorf("TraitModel agent must not appear in cluster output\ngot: %s", out)
	}
	if !strings.Contains(out, "ocr-agent") {
		t.Errorf("non-model agent must appear in cluster output\ngot: %s", out)
	}
}

// TestBuildCapabilityCluster_Uncategorized verifies agents with no Capabilities
// fall back to description-derived capability label (REQ-CLUSTER-3).
func TestBuildCapabilityCluster_Uncategorized(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "file-writer", Description: "Generic file writer", Capabilities: nil},
	}

	out := buildCapabilityCluster(agents)

	// REQ-CLUSTER-3: empty capabilities → description-derived label, not "(uncategorized)"
	if strings.Contains(out, "(uncategorized)") {
		t.Errorf("output should not contain (uncategorized) when Description is present\ngot: %s", out)
	}
	if !strings.Contains(out, "file-writer") {
		t.Errorf("output missing file-writer ID\ngot: %s", out)
	}
	if !strings.Contains(out, "Generic file writer") {
		t.Errorf("output missing description-derived label\ngot: %s", out)
	}
}

// TestBuildCapabilityCluster_SingleCluster is the tracer bullet: agents sharing
// one capability appear under that capability heading.
func TestBuildCapabilityCluster_SingleCluster(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "ocr-agent", Capabilities: []string{"vision"}},
		{ID: "screenshot-agent", Capabilities: []string{"vision"}},
	}

	out := buildCapabilityCluster(agents)

	if !strings.Contains(out, "CAPABILITY CLUSTERS") {
		t.Errorf("output missing CAPABILITY CLUSTERS header\ngot: %s", out)
	}
	if !strings.Contains(out, "vision:") {
		t.Errorf("output missing 'vision:' cluster heading\ngot: %s", out)
	}
	if !strings.Contains(out, "ocr-agent") {
		t.Errorf("output missing ocr-agent\ngot: %s", out)
	}
	if !strings.Contains(out, "screenshot-agent") {
		t.Errorf("output missing screenshot-agent\ngot: %s", out)
	}
}
