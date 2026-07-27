package mcp

import "testing"

// Classification tags on MCP tools (ADR-0085 D2 / ADR-0090).
//
// The gap these close: a remote server's tools are discovered dynamically, so
// without an operator-set default they arrive UNTAGGED — and an untagged resource
// has no tags for a policy to forbid. "Only this path may use these tools" could
// not be expressed at all, whatever policy was written.
//
// `discover` needs a live MCP session, so these test the policy-application rule
// directly: server default, per-tool override, and who is allowed to decide.

// applyClassification mirrors the rule inside discover: the server default
// applies, and a per-tool list REPLACES rather than extends it.
func applyClassification(toolName string, policy map[string]ToolPolicy, defaultTags []string) []string {
	tags := defaultTags
	if p, ok := policy[toolName]; ok && len(p.ClassificationTags) > 0 {
		tags = p.ClassificationTags
	}
	return tags
}

func TestClassification_ServerDefaultAppliesToEveryTool(t *testing.T) {
	policy := map[string]ToolPolicy{}
	for _, tool := range []string{"book_reservation", "cancel_reservation", "search_flights"} {
		got := applyClassification(tool, policy, []string{"airline"})
		if len(got) != 1 || got[0] != "airline" {
			t.Errorf("%s: expected the server default, got %v", tool, got)
		}
	}
}

// A per-tool list REPLACES the server default rather than adding to it, so one
// tool can leave a domain boundary without silently inheriting it.
func TestClassification_PerToolOverridesRatherThanExtends(t *testing.T) {
	policy := map[string]ToolPolicy{
		"public_status": {ClassificationTags: []string{"public"}},
	}
	got := applyClassification("public_status", policy, []string{"airline"})
	if len(got) != 1 || got[0] != "public" {
		t.Fatalf("expected the per-tool tags to replace the default, got %v", got)
	}
	// Everything else still inherits.
	if other := applyClassification("book_reservation", policy, []string{"airline"}); len(other) != 1 || other[0] != "airline" {
		t.Errorf("other tools must keep the default, got %v", other)
	}
}

// An empty per-tool list is not an override — it is the absence of one. Treating
// it as "clear the tags" would let a policy entry written for pricing silently
// strip a domain boundary.
func TestClassification_EmptyPerToolListDoesNotClearTheDefault(t *testing.T) {
	policy := map[string]ToolPolicy{
		"book_reservation": {Dangerous: true}, // present, but says nothing about tags
	}
	got := applyClassification("book_reservation", policy, []string{"airline"})
	if len(got) != 1 || got[0] != "airline" {
		t.Errorf("a policy entry without tags must not strip the default, got %v", got)
	}
}

// No default and no override: untagged, which is today's behaviour for every MCP
// server and is why the boundary was previously inexpressible.
func TestClassification_NoTagsIsStillPossible(t *testing.T) {
	if got := applyClassification("anything", map[string]ToolPolicy{}, nil); len(got) != 0 {
		t.Errorf("expected no tags, got %v", got)
	}
}

// The tags live on ToolPolicy — operator-set — not on anything the server sends.
// A remote server classifying its own tools would be the governed choosing its
// own governance, the same reason it does not get to declare itself safe.
func TestClassification_IsOperatorSet(t *testing.T) {
	p := ToolPolicy{ClassificationTags: []string{"airline"}}
	if len(p.ClassificationTags) != 1 {
		t.Fatal("ToolPolicy must carry classification tags")
	}
	var cfg ServerConfig
	cfg.DefaultClassificationTags = []string{"airline"}
	if len(cfg.DefaultClassificationTags) != 1 {
		t.Fatal("ServerConfig must carry a default for dynamically discovered tools")
	}
}
