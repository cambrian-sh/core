package scope_test

import (
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"
)

// C2: an agent can never broaden — a hint tag outside DefaultWriteTags is dropped.
func TestArtifact_WriteCannotBroaden(t *testing.T) {
	vocab := scope.NewVocabulary([]string{"secrets", "public_kb"})
	ws := scope.WriterScope{WriterID: "support", DefaultWriteTags: []string{"public_kb"}}

	tags, err := scope.AuthorizeArtifactWrite(ws, vocab, []string{"secrets"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if tg == "secrets" {
			t.Fatalf("agent must not be able to classify as secrets, got %v", tags)
		}
	}
}

func TestArtifact_WriteRejectsCoinage(t *testing.T) {
	vocab := scope.NewVocabulary([]string{"public_kb"})
	ws := scope.WriterScope{WriterID: "a", DefaultWriteTags: []string{"public_kb"}}

	if _, err := scope.AuthorizeArtifactWrite(ws, vocab, []string{"made_up"}); !errors.Is(err, scope.ErrUnknownClassification) {
		t.Fatalf("expected ErrUnknownClassification, got %v", err)
	}
}

func TestArtifact_WriteDerivesAndStampsProvenance(t *testing.T) {
	vocab := scope.NewVocabulary([]string{"public_kb"})
	ws := scope.WriterScope{WriterID: "writer1", DefaultWriteTags: []string{"public_kb"}}

	tags, err := scope.AuthorizeArtifactWrite(ws, vocab, nil) // no hint → full DefaultWriteTags
	if err != nil {
		t.Fatal(err)
	}
	var hasClass, hasProv bool
	for _, tg := range tags {
		if tg == "public_kb" {
			hasClass = true
		}
		if tg == "provenance:source=writer1" {
			hasProv = true
		}
	}
	if !hasClass || !hasProv {
		t.Errorf("expected derived classification + kernel provenance, got %v", tags)
	}
}

func TestArtifact_ReadFilterExcludesSecrets(t *testing.T) {
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, domain.ScopeConfig{ForbiddenTags: []string{"secrets"}})
	artifacts := []domain.Artifact{
		{Hash: "a", Tags: []string{"public_kb"}},
		{Hash: "b", Tags: []string{"secrets"}},
	}
	got := scope.FilterArtifactsByScope(&eff, artifacts)
	if len(got) != 1 || got[0].Hash != "a" {
		t.Errorf("support agent must not see secrets artifact, got %+v", got)
	}
	if scope.ArtifactReadable(&eff, artifacts[1]) {
		t.Errorf("secrets artifact must not be readable")
	}
}

func TestArtifact_ReadNilScopeFailsClosed(t *testing.T) {
	artifacts := []domain.Artifact{{Hash: "a", Tags: []string{"public_kb"}}}
	if got := scope.FilterArtifactsByScope(nil, artifacts); len(got) != 0 {
		t.Errorf("nil scope must be fail-closed, got %+v", got)
	}
}

func TestArtifact_ContextRefsScopeFiltered(t *testing.T) {
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, domain.ScopeConfig{ForbiddenTags: []string{"secrets"}})
	arts := []domain.Artifact{
		{Hash: "ok", Tags: []string{"public_kb"}, SemanticSummary: "a chart"},
		{Hash: "secret", Tags: []string{"secrets"}},
	}
	refs := scope.ArtifactContextRefs(&eff, arts, "step_1")
	if len(refs) != 1 || string(refs[0].CID) != "ok" || refs[0].Type != "agent_artifact" {
		t.Fatalf("expected one scope-visible artifact ref, got %+v", refs)
	}
	if !contains(refs[0].Labels, "step_1") {
		t.Errorf("ref must carry the step label, got %v", refs[0].Labels)
	}
}
