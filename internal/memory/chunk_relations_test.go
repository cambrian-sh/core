package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChunkRelations_Struct(t *testing.T) {
	cr := ChunkRelations{
		ParentEntityID:   "ent_abc123",
		PrecedingChunkID: "chk_prev",
		FollowingChunkID: "chk_next",
		SiblingContext: SiblingContext{
			ParentTitle:      "Cambrian Architecture",
			ParentSummary:    "Overview of the runtime",
			ParentScene:      "Operator setting up the kernel",
			PrecedingSnippet: "First chunk body",
			FollowingSnippet: "Last chunk body",
		},
	}

	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ChunkRelations
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ParentEntityID != cr.ParentEntityID {
		t.Errorf("ParentEntityID: got %q, want %q", got.ParentEntityID, cr.ParentEntityID)
	}
	if got.PrecedingChunkID != cr.PrecedingChunkID {
		t.Errorf("PrecedingChunkID: got %q, want %q", got.PrecedingChunkID, cr.PrecedingChunkID)
	}
	if got.FollowingChunkID != cr.FollowingChunkID {
		t.Errorf("FollowingChunkID: got %q, want %q", got.FollowingChunkID, cr.FollowingChunkID)
	}
	if got.SiblingContext.ParentTitle != cr.SiblingContext.ParentTitle {
		t.Errorf("ParentTitle: got %q, want %q", got.SiblingContext.ParentTitle, cr.SiblingContext.ParentTitle)
	}
	if got.SiblingContext.ParentSummary != cr.SiblingContext.ParentSummary {
		t.Errorf("ParentSummary: got %q, want %q", got.SiblingContext.ParentSummary, cr.SiblingContext.ParentSummary)
	}
	if got.SiblingContext.ParentScene != cr.SiblingContext.ParentScene {
		t.Errorf("ParentScene: got %q, want %q", got.SiblingContext.ParentScene, cr.SiblingContext.ParentScene)
	}
	if got.SiblingContext.PrecedingSnippet != cr.SiblingContext.PrecedingSnippet {
		t.Errorf("PrecedingSnippet: got %q, want %q", got.SiblingContext.PrecedingSnippet, cr.SiblingContext.PrecedingSnippet)
	}
	if got.SiblingContext.FollowingSnippet != cr.SiblingContext.FollowingSnippet {
		t.Errorf("FollowingSnippet: got %q, want %q", got.SiblingContext.FollowingSnippet, cr.SiblingContext.FollowingSnippet)
	}
}

func TestChunkRelations_BudgetEnforced(t *testing.T) {
	big := strings.Repeat("a", 500)
	sc := SiblingContext{
		ParentTitle:      big,
		ParentSummary:    big,
		ParentScene:      big,
		PrecedingSnippet: big,
		FollowingSnippet: big,
	}

	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := len(data); got > siblingContextMaxBytes {
		t.Errorf("marshaled JSON %d bytes, want <= %d (siblingContextMaxBytes)", got, siblingContextMaxBytes)
	}
}

func TestChunkRelations_PerFieldLimits(t *testing.T) {
	big := strings.Repeat("b", 500)
	sc := SiblingContext{
		ParentTitle:      big,
		ParentSummary:    big,
		ParentScene:      big,
		PrecedingSnippet: big,
		FollowingSnippet: big,
	}

	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got SiblingContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if l := len(got.ParentTitle); l > parentTitleMaxBytes {
		t.Errorf("ParentTitle: %d bytes, want <= %d", l, parentTitleMaxBytes)
	}
	if l := len(got.ParentSummary); l > parentSummaryMaxBytes {
		t.Errorf("ParentSummary: %d bytes, want <= %d", l, parentSummaryMaxBytes)
	}
	if l := len(got.ParentScene); l > parentSceneMaxBytes {
		t.Errorf("ParentScene: %d bytes, want <= %d", l, parentSceneMaxBytes)
	}
	if l := len(got.PrecedingSnippet); l > precedingSnippetMaxBytes {
		t.Errorf("PrecedingSnippet: %d bytes, want <= %d", l, precedingSnippetMaxBytes)
	}
	if l := len(got.FollowingSnippet); l > followingSnippetMaxBytes {
		t.Errorf("FollowingSnippet: %d bytes, want <= %d", l, followingSnippetMaxBytes)
	}
}

func TestChunkRelations_WordBoundaryTruncation(t *testing.T) {
	s := "This is a long string with several words that should be truncated at the last word boundary within the byte limit"
	got := truncateAtWordBoundary(s, 50)
	if len(got) > 50 {
		t.Errorf("truncateAtWordBoundary returned %d bytes, want <= 50; got=%q", len(got), got)
	}
	if strings.HasSuffix(got, " ") {
		t.Errorf("truncateAtWordBoundary should not end with a space: %q", got)
	}
	if !strings.HasPrefix(s, got) {
		t.Errorf("truncateAtWordBoundary result %q is not a prefix of %q", got, s)
	}
	if got == "" {
		t.Errorf("truncateAtWordBoundary returned empty for non-empty input")
	}
	if strings.Contains(got, "should") || strings.Contains(got, "boundary") {
		t.Errorf("truncateAtWordBoundary should have cut before 'should': %q", got)
	}
}

func TestChunkRelations_HardCutFallback(t *testing.T) {
	s := strings.Repeat("a", 200)
	got := truncateAtWordBoundary(s, 80)
	if len(got) > 80 {
		t.Errorf("hard-cut returned %d bytes, want <= 80; got=%q", len(got), got)
	}
	if got != s[:len(got)] {
		t.Errorf("hard-cut result is not a prefix of input: %q", got)
	}
	if got != s[:80] {
		t.Errorf("hard-cut expected exactly s[:80] for pure-ASCII no-space input, got %q (len=%d)", got, len(got))
	}
}

func TestChunkRelations_EmptyContext(t *testing.T) {
	sc := SiblingContext{}
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := len(data); got > siblingContextMaxBytes {
		t.Errorf("empty context marshaled to %d bytes, want <= %d", got, siblingContextMaxBytes)
	}
	if string(data) != "{}" {
		t.Errorf("empty SiblingContext should marshal to %q, got %q", "{}", string(data))
	}
}

func TestChunkRelations_OnlyIdentifiers(t *testing.T) {
	cr := ChunkRelations{
		ParentEntityID: "ent_abc123",
	}

	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := len(data); got > siblingContextMaxBytes {
		t.Errorf("identifier-only chunk_relations marshaled to %d bytes, want <= %d", got, siblingContextMaxBytes)
	}

	var got ChunkRelations
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ParentEntityID != "ent_abc123" {
		t.Errorf("ParentEntityID: got %q, want %q", got.ParentEntityID, "ent_abc123")
	}
	if got.SiblingContext.ParentTitle != "" {
		t.Errorf("ParentTitle should be empty for identifier-only payload, got %q", got.SiblingContext.ParentTitle)
	}
}
