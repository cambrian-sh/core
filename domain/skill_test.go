package domain

import (
	"context"
	"strings"
	"testing"
)

// The system-skill registry registers, gets, lists, and upserts by name.
func TestInMemorySkillRegistry(t *testing.T) {
	r := NewInMemorySkillRegistry()
	r.Register(Skill{Name: "deploy", Description: "Deploy the app."})

	if s, ok := r.Get("deploy"); !ok || s.Name != "deploy" {
		t.Fatalf("Get(deploy) = %+v, %v", s, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get of an unregistered skill should be !ok")
	}
	if len(r.All()) != 1 {
		t.Errorf("All() should have 1 skill, got %d", len(r.All()))
	}

	r.Register(Skill{Name: "deploy", Description: "Deploy v2."}) // re-register
	if len(r.All()) != 1 {
		t.Errorf("re-register must upsert (1 skill), got %d", len(r.All()))
	}
}

// BuildSkillDoc carries name + the short summary (Tier-1), document-prefixed, and
// reuses the ADR-0045 deriver — so only the first sentence is embedded.
func TestBuildSkillDoc(t *testing.T) {
	doc := BuildSkillDoc(Skill{
		Name:        "deploy_app",
		Description: "Deploy the app to prod. Runs migrations and health checks.",
	})
	if !strings.HasPrefix(doc, "search_document: ") {
		t.Errorf("doc must carry the asymmetric document prefix, got %q", doc)
	}
	for _, want := range []string{"deploy_app", "Deploy the app to prod."} {
		if !strings.Contains(doc, want) {
			t.Errorf("doc should contain %q, got %q", want, doc)
		}
	}
	if strings.Contains(doc, "health checks") {
		t.Errorf("doc should carry only the short summary (first sentence), got %q", doc)
	}
}

// The skill indexer tags the doc's metadata.tags with the skill's scope labels —
// the key the ADR-0034 scope predicate filters on (option A).
func TestSkillIndexer_TagsDocWithScope(t *testing.T) {
	store := &fakeToolStore{}
	ix := &SkillIndexer{Store: store, Embedder: &fakeEmbedder{}}
	if err := ix.Index(context.Background(), Skill{Name: "deploy", Description: "Deploy.", ScopeTags: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	doc := store.saved["deploy"]
	if doc == nil || doc.DocumentType != DocTypeSkill {
		t.Fatalf("skill indexed under wrong type: %+v", doc)
	}
	tags, _ := doc.Metadata["tags"].([]string)
	if len(tags) != 1 || tags[0] != "ops" {
		t.Errorf("skill doc should carry metadata.tags=[ops] for scope filtering, got %v", doc.Metadata["tags"])
	}
}

// SkillDisclosure: Tier-1 is summary-only; Tier-2 adds instructions + grants.
func TestSkillDisclosure(t *testing.T) {
	s := Skill{
		Name: "deploy", Description: "Deploy the app. Runs migrations.",
		Instructions: "1. migrate\n2. ship", ToolGrants: []string{"execute_command"},
	}
	desc, instr, grants := SkillDisclosure(s, false)
	if desc != "Deploy the app." || instr != "" || grants != nil {
		t.Errorf("Tier-1 should be summary-only, got desc=%q instr=%q grants=%v", desc, instr, grants)
	}
	desc, instr, grants = SkillDisclosure(s, true)
	if instr != "1. migrate\n2. ship" || len(grants) != 1 {
		t.Errorf("Tier-2 should carry instructions + grants, got instr=%q grants=%v", instr, grants)
	}
}

// SkillVisible: nil scope denies (fail-closed); ScopeSystem permits; otherwise
// EffectiveScope.Allows decides on the skill's tags.
func TestSkillVisible(t *testing.T) {
	s := Skill{Name: "deploy", ScopeTags: []string{"ops"}}
	if SkillVisible(nil, s) {
		t.Error("nil scope must be fail-closed (deny)")
	}
	if !SkillVisible(ScopeSystem, s) {
		t.Error("ScopeSystem must permit")
	}
	permits := &EffectiveScope{AnyOfClauses: [][]string{{"ops"}}}
	if !SkillVisible(permits, s) {
		t.Error("a scope permitting 'ops' should see an ops-tagged skill")
	}
	denies := &EffectiveScope{AnyOfClauses: [][]string{{"finance"}}}
	if SkillVisible(denies, s) {
		t.Error("a scope not permitting 'ops' must not see an ops-tagged skill")
	}
}

// VectorSkillRetriever queries DocTypeSkill, applies the search_query prefix and
// the relevance floor, and passes the agent's effective scope to the store (the
// ADR-0034 scope-gating chokepoint).
func TestVectorSkillRetriever_FloorAndScopePassed(t *testing.T) {
	emb := &fakeEmbedder{}
	store := &fakeToolStore{results: []SearchResult{
		{Document: Document{ID: "deploy"}, RawScore: 0.70},
		{Document: Document{ID: "backup"}, RawScore: 0.20}, // below floor
	}}
	r := VectorSkillRetriever{Store: store, Embedder: emb, Floor: 0.30}
	scope := &EffectiveScope{AnyOfClauses: [][]string{{"ops"}}}

	got, err := r.Rank(context.Background(), "deploy the app", scope, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "deploy" {
		t.Errorf("floor should drop the 0.20 result, got %v", got)
	}
	if !strings.HasPrefix(emb.lastText, "search_query: ") {
		t.Errorf("query must carry the asymmetric query prefix, got %q", emb.lastText)
	}
	if store.lastDocType != DocTypeSkill {
		t.Errorf("retriever must query DocTypeSkill, got %q", store.lastDocType)
	}
	if store.lastScope != scope {
		t.Error("agent effective scope must be passed to the store for ADR-0034 gating")
	}
}
