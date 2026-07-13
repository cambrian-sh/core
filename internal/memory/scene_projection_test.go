package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0049 D7: refKind abstracts an engaged ref's prefix to an entity CATEGORY so the
// retrieval projection matches on shape, not identity.
func TestRefKind(t *testing.T) {
	cases := map[string]string{
		"path:a.md":        "file",
		"file:/etc/hosts":  "file",
		"dir:/tmp":         "directory",
		"url:http://x":     "web resource",
		"host:example.com": "web resource",
		"command:ls":       "shell command",
		"weird:thing":      "resource",
		"nocolon":          "resource",
	}
	for ref, want := range cases {
		if got := refKind(ref); got != want {
			t.Errorf("refKind(%q) = %q; want %q", ref, got, want)
		}
	}
}

// ADR-0049 D7: the projection is the ABSTRACTED retrieval face — goal + engaged KIND
// counts, with specific IDs and the outcome deliberately EXCLUDED (those are an index
// field / a post-match field, not part of the similarity key).
func TestSceneProjection_AbstractsToKindCounts(t *testing.T) {
	proj := sceneProjection("build docs", []string{"path:a.md", "path:b.md", "url:http://x"})

	if !strings.Contains(proj, "goal: build docs") {
		t.Errorf("projection must carry the goal; got %q", proj)
	}
	if !strings.Contains(proj, "2 file") || !strings.Contains(proj, "1 web resource") {
		t.Errorf("projection must abstract to KIND counts (2 file, 1 web resource); got %q", proj)
	}
	// Identity must NOT leak into the similarity key — that defeats situational matching.
	if strings.Contains(proj, "a.md") || strings.Contains(proj, "http://x") {
		t.Errorf("projection must not contain specific IDs; got %q", proj)
	}
	// Outcome is a post-match field, never part of what "situations like this" matches.
	if strings.Contains(proj, "success") || strings.Contains(proj, "failure") {
		t.Errorf("projection must not encode outcome; got %q", proj)
	}
}

// ADR-0049 D7: a replan's "Replan: <orig>" goal must normalize to the original situation
// so the failed original and its replan project to the SAME scene situation.
func TestNormalizeSceneGoal_StripsReplanPrefix(t *testing.T) {
	if got := normalizeSceneGoal("Replan: build the docs"); got != "build the docs" {
		t.Errorf("Replan prefix must be stripped; got %q", got)
	}
	if got := normalizeSceneGoal("Replan: Replan: build the docs"); got != "build the docs" {
		t.Errorf("nested Replan prefixes must all be stripped; got %q", got)
	}
	if got := normalizeSceneGoal("build the docs"); got != "build the docs" {
		t.Errorf("a normal goal must be unchanged; got %q", got)
	}
}

// A goal with no engaged refs projects to just the goal (no engages clause).
func TestSceneProjection_NoRefs(t *testing.T) {
	proj := sceneProjection("ponder", nil)
	if proj != "goal: ponder" {
		t.Errorf("no-ref projection must be the bare goal; got %q", proj)
	}
}

// ADR-0049 D7: WritePlanScene embeds the ABSTRACTED projection (not the reconstruction
// Text), and records the outcome as a metadata FIELD (read after a situational match).
func TestWritePlanScene_EmbedsProjectionAndRecordsOutcome(t *testing.T) {
	store := &captureSaveStore{}
	emb := &recordingEmbedder{}
	agent := NewAgent(NewMemoryManager(store, emb), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()

	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-pf"})
	_ = agent.WritePlanScene(ctx, "pf", "ship it", false) // failed plan

	// The EMBEDDED text is the abstracted projection, not the human-readable Text.
	if emb.lastText != "goal: ship it | engages: 1 file" {
		t.Errorf("scene must embed the abstracted projection; embedded %q", emb.lastText)
	}
	if store.savedDoc.Text == emb.lastText {
		t.Error("the reconstruction Text and the embedded projection must differ (two faces)")
	}
	if got := store.savedDoc.Metadata["outcome"]; got != "failure" {
		t.Errorf("outcome must be recorded as a metadata field; got %v", got)
	}
	if store.savedDoc.Metadata["projection"] != emb.lastText {
		t.Errorf("metadata projection must equal the embedded text; got %v", store.savedDoc.Metadata["projection"])
	}
}
