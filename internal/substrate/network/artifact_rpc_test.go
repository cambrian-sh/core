package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- fakes ---

type fakeVault struct{ data map[string][]byte }

func newFakeVault() *fakeVault { return &fakeVault{data: map[string][]byte{}} }
func (f *fakeVault) Store(content []byte) (string, error) {
	h := "h" + string(rune(len(f.data)+'0'))
	f.data[h] = content
	return h, nil
}
func (f *fakeVault) Load(hash string) ([]byte, error) { return f.data[hash], nil }

type fakeMetaStore struct{ recs map[string]domain.Artifact }

func newFakeMeta() *fakeMetaStore { return &fakeMetaStore{recs: map[string]domain.Artifact{}} }
func (f *fakeMetaStore) SaveArtifact(a domain.Artifact) error {
	f.recs[a.Hash] = a
	return nil
}
func (f *fakeMetaStore) GetArtifact(hash string) (*domain.Artifact, error) {
	if a, ok := f.recs[hash]; ok {
		return &a, nil
	}
	return nil, nil
}
func (f *fakeMetaStore) ListStepArtifacts(session string, step int) ([]domain.Artifact, error) {
	var out []domain.Artifact
	for _, a := range f.recs {
		if a.SessionID == session && a.StepIndex == step {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeArtScopes struct {
	scopes    map[string]domain.ScopeConfig
	writeTags map[string][]string
}

func (f fakeArtScopes) EffectiveForAgent(_ context.Context, id string) (*domain.EffectiveScope, bool) {
	cfg, ok := f.scopes[id]
	if !ok {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, cfg)
	return &eff, true
}

func (f fakeArtScopes) EffectiveForCaller(_ context.Context, id string, caller domain.ScopeConfig) (*domain.EffectiveScope, bool) {
	cfg, ok := f.scopes[id]
	if !ok {
		return nil, false
	}
	eff := domain.NewEffectiveScope(caller, cfg)
	return &eff, true
}

func (f fakeArtScopes) DefaultWriteTags(_ context.Context, id string) []string {
	return f.writeTags[id]
}

func agentCtx(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", id))
}

func newArtifactServer(scopes map[string]domain.ScopeConfig, writeTags map[string][]string, vocab []string) (*Server, *fakeMetaStore) {
	meta := newFakeMeta()
	return &Server{
		ArtifactBytes:  newFakeVault(),
		ArtifactMeta:   meta,
		ArtifactScopes: fakeArtScopes{scopes: scopes, writeTags: writeTags},
		ArtifactVocab:  scope.NewVocabulary(vocab),
	}, meta
}

// C2: an agent cannot classify an upload as anything outside its DefaultWriteTags.
func TestUploadArtifact_CannotBroaden(t *testing.T) {
	s, meta := newArtifactServer(
		map[string]domain.ScopeConfig{"support": {}},
		map[string][]string{"support": {"public_kb"}},
		[]string{"secrets", "public_kb"})

	resp, err := s.UploadArtifact(agentCtx("support"), &pb.UploadArtifactRequest{
		Content: []byte("x"), Tags: []string{"secrets"}, // tries to classify as secrets
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range meta.recs[resp.Hash].Tags {
		if tg == "secrets" {
			t.Fatalf("agent must not classify an artifact as secrets, got %v", meta.recs[resp.Hash].Tags)
		}
	}
}

func TestUploadArtifact_RejectsCoinage(t *testing.T) {
	s, _ := newArtifactServer(
		map[string]domain.ScopeConfig{"a": {}},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb"})
	_, err := s.UploadArtifact(agentCtx("a"), &pb.UploadArtifactRequest{
		Content: []byte("x"), Tags: []string{"invented"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestUploadArtifact_DerivesClassificationAndStampsProvenance(t *testing.T) {
	s, meta := newArtifactServer(
		map[string]domain.ScopeConfig{"a": {}},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb"})
	resp, err := s.UploadArtifact(agentCtx("a"), &pb.UploadArtifactRequest{
		Content: []byte("hello"), // no hint → full DefaultWriteTags
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := meta.recs[resp.Hash]
	var hasClass, hasProv bool
	for _, tg := range stored.Tags {
		if tg == "public_kb" {
			hasClass = true
		}
		if tg == "provenance:source=a" {
			hasProv = true
		}
	}
	if !hasClass || !hasProv {
		t.Errorf("expected derived classification + provenance, got %v", stored.Tags)
	}
}

func TestGetArtifact_ScopeDeniedReportsNotFound(t *testing.T) {
	s, meta := newArtifactServer(map[string]domain.ScopeConfig{
		"support": {ForbiddenTags: []string{"secrets"}},
	}, nil, []string{"secrets"})
	meta.recs["h0"] = domain.Artifact{Hash: "h0", Tags: []string{"secrets"}}

	resp, err := s.GetArtifact(agentCtx("support"), &pb.GetArtifactRequest{Hash: "h0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Found {
		t.Errorf("a scope-denied artifact must report found=false (no existence leak)")
	}
}

func TestListStepArtifacts_FiltersByScope(t *testing.T) {
	s, meta := newArtifactServer(map[string]domain.ScopeConfig{
		"support": {ForbiddenTags: []string{"secrets"}},
	}, nil, []string{"secrets", "public_kb"})
	meta.recs["h0"] = domain.Artifact{Hash: "h0", SessionID: "s", StepIndex: 1, Tags: []string{"public_kb"}}
	meta.recs["h1"] = domain.Artifact{Hash: "h1", SessionID: "s", StepIndex: 1, Tags: []string{"secrets"}}

	resp, err := s.ListStepArtifacts(agentCtx("support"), &pb.ListStepArtifactsRequest{SessionId: "s", StepIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Hash != "h0" {
		t.Errorf("support agent must see only the public artifact, got %+v", resp.Artifacts)
	}
}
