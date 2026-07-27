package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

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

// fakeArtAuthz is a stand-in decision point for the artifact plane: a fixed
// predicate per principal plus a write ceiling, with the narrow-only and
// controlled-vocabulary rules the real one enforces. It exists so these tests
// assert the KERNEL's behaviour (does the RPC ask, and does it honour the
// answer) without importing the premium implementation.
type fakeArtAuthz struct {
	domain.AllowAllAuthorizer
	preds     map[string]*domain.TagPredicate
	writeTags map[string][]string
	vocab     map[string]bool
}

func (f fakeArtAuthz) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	pred, ok := f.preds[p.ID]
	if !ok {
		return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonNoPrincipal}
	}
	return pred, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func (f fakeArtAuthz) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	for _, h := range hint {
		if len(f.vocab) > 0 && !f.vocab[h] {
			return nil, domain.AccessDecision{Principal: p, Reason: domain.ReasonForbiddenTag, Detail: h}
		}
	}
	ceiling := f.writeTags[p.ID]
	out := []string{}
	if len(hint) == 0 {
		out = append(out, ceiling...)
	} else {
		want := map[string]bool{}
		for _, h := range hint {
			want[h] = true
		}
		for _, c := range ceiling { // narrow-only: the hint may remove, never add
			if want[c] {
				out = append(out, c)
			}
		}
	}
	return append(out, "provenance:source="+p.ID), domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func agentCtx(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", id))
}

func newArtifactServer(preds map[string]*domain.TagPredicate, writeTags map[string][]string, vocab []string) (*Server, *fakeMetaStore) {
	meta := newFakeMeta()
	vset := map[string]bool{}
	for _, v := range vocab {
		vset[v] = true
	}
	return &Server{
		ArtifactBytes: newFakeVault(),
		ArtifactMeta:  meta,
		Authz:         fakeArtAuthz{preds: preds, writeTags: writeTags, vocab: vset},
	}, meta
}

// C2: an agent cannot classify an upload as anything outside its DefaultWriteTags.
func TestUploadArtifact_CannotBroaden(t *testing.T) {
	s, meta := newArtifactServer(
		map[string]*domain.TagPredicate{"support": {}},
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
		map[string]*domain.TagPredicate{"a": {}},
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
		map[string]*domain.TagPredicate{"a": {}},
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
	s, meta := newArtifactServer(map[string]*domain.TagPredicate{
		"support": {ForbiddenTags: []string{"secrets"}},
	}, nil, []string{"secrets"})
	meta.recs["h0"] = domain.Artifact{Hash: "h0", Tags: []string{"secrets"}}

	resp, err := s.GetArtifact(agentCtx("support"), &pb.GetArtifactRequest{Hash: "h0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Found {
		t.Errorf("a denied artifact must report found=false (no existence leak)")
	}
}

func TestListStepArtifacts_FiltersByScope(t *testing.T) {
	s, meta := newArtifactServer(map[string]*domain.TagPredicate{
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
