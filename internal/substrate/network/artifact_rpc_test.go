package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ledger"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- fakes ---

// newArtifactLedger is the run ledger the artifact plane serves from: the REAL
// classified writer (ledger.Recorder) and the REAL enforced reader
// (authz.EnforcingLedger) over an in-memory raw store, both asking authorizer —
// exactly how kernel.NewLedgerStack assembles them.
func newArtifactLedger(authorizer domain.Authorizer) (*LedgerAccess, *memLedgerStore) {
	raw := &memLedgerStore{rows: map[string]domain.LedgerEntry{}}
	return &LedgerAccess{
		Writer: ledger.NewRecorder(raw, authorizer, ledger.RecorderConfig{MaxArtifactBytes: 64}),
		Reader: authz.NewEnforcingLedger(raw, nil),
	}, raw
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

func newArtifactServer(preds map[string]*domain.TagPredicate, writeTags map[string][]string, vocab []string) (*Server, *memLedgerStore) {
	vset := map[string]bool{}
	for _, v := range vocab {
		vset[v] = true
	}
	az := fakeArtAuthz{preds: preds, writeTags: writeTags, vocab: vset}
	led, raw := newArtifactLedger(az)
	return &Server{Ledger: led, Authz: az}, raw
}

// C2: an agent cannot classify an upload as anything outside its DefaultWriteTags.
func TestUploadArtifact_CannotBroaden(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(
		map[string]*domain.TagPredicate{"support": {}},
		map[string][]string{"support": {"public_kb"}},
		[]string{"secrets", "public_kb"})

	resp, err := s.UploadArtifact(agentCtx("support"), &pb.UploadArtifactRequest{
		Content: []byte("x"), Tags: []string{"secrets"}, // tries to classify as secrets
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range raw.rows[resp.Hash].Tags {
		if tg == "secrets" {
			t.Fatalf("agent must not classify an artifact as secrets, got %v", raw.rows[resp.Hash].Tags)
		}
	}
}

func TestUploadArtifact_RejectsCoinage(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(
		map[string]*domain.TagPredicate{"a": {}},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb"})
	_, err := s.UploadArtifact(agentCtx("a"), &pb.UploadArtifactRequest{
		Content: []byte("x"), Tags: []string{"invented"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if len(raw.rows) != 0 {
		t.Fatalf("a refused upload was recorded: %v", raw.rows)
	}
}

func TestUploadArtifact_DerivesClassificationAndStampsProvenance(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(
		map[string]*domain.TagPredicate{"a": {}},
		map[string][]string{"a": {"public_kb"}},
		[]string{"public_kb"})
	// The instance comes from the verified context (the verifier stamps it), never
	// from the request; the request's session/step are ignored (INV-5).
	ctx := domain.WithAgentInstance(agentCtx("a"), "inst-a1")
	resp, err := s.UploadArtifact(ctx, &pb.UploadArtifactRequest{
		Content: []byte("hello"), SessionId: "forged-session", StepIndex: 9, // no hint → full DefaultWriteTags
	})
	if err != nil {
		t.Fatal(err)
	}
	if !domain.IsLedgerID(resp.Hash) {
		t.Fatalf("UploadArtifact must answer a ledger id, got %q", resp.Hash)
	}
	stored := raw.rows[resp.Hash]
	if stored.Kind != domain.LedgerArtifact || stored.Writer != "agent:a" || stored.InstanceID != "inst-a1" {
		t.Fatalf("artifact entry not attributed to the verified writer and instance: %+v", stored)
	}
	if stored.SessionID == "forged-session" || stored.StepIndex == 9 {
		t.Fatalf("provenance taken from the request payload: %+v", stored)
	}
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

// T-20261115-artifact-metadata-is-keyed-by-content-hash-so: the same bytes uploaded
// by two owners stay TWO artifacts, each with its own tags; the second upload does
// not overwrite the first owner's classification, and each owner reads only theirs.
func TestUploadArtifact_SameBytesTwoOwnersStayTwoEntries(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(map[string]*domain.TagPredicate{
		"alice_agent": {RequiredTags: []string{"team_alice"}},
		"bob_agent":   {RequiredTags: []string{"team_bob"}},
	}, map[string][]string{
		"alice_agent": {"team_alice"},
		"bob_agent":   {"team_bob"},
	}, []string{"team_alice", "team_bob"})
	body := []byte("quarterly figures")
	ra, err := s.UploadArtifact(agentCtx("alice_agent"), &pb.UploadArtifactRequest{Content: body})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := s.UploadArtifact(agentCtx("bob_agent"), &pb.UploadArtifactRequest{Content: body})
	if err != nil {
		t.Fatal(err)
	}
	if ra.Hash == rb.Hash {
		t.Fatalf("two owners' uploads of the same bytes share one entry %q", ra.Hash)
	}
	if !hasTag(raw.rows[ra.Hash].Tags, "team_alice") || hasTag(raw.rows[ra.Hash].Tags, "team_bob") {
		t.Fatalf("alice's entry lost its tags: %v", raw.rows[ra.Hash].Tags)
	}
	if !hasTag(raw.rows[rb.Hash].Tags, "team_bob") {
		t.Fatalf("bob's entry tags: %v", raw.rows[rb.Hash].Tags)
	}
	got, err := s.GetArtifact(agentCtx("alice_agent"), &pb.GetArtifactRequest{Hash: ra.Hash})
	if err != nil || !got.Found || string(got.Content) != string(body) {
		t.Fatalf("alice cannot read her own artifact: %+v %v", got, err)
	}
	// A receiver outside the entry's scope gets found=false.
	if got, err := s.GetArtifact(agentCtx("bob_agent"), &pb.GetArtifactRequest{Hash: ra.Hash}); err != nil || got.Found {
		t.Fatalf("bob read alice's artifact: %+v %v", got, err)
	}
}

// An artifact over execution.ledger.artifacts.max_file_bytes is refused by name,
// never truncated.
func TestUploadArtifact_SizeCapRefused(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(map[string]*domain.TagPredicate{"a": {}}, map[string][]string{"a": {"public_kb"}}, nil)
	_, err := s.UploadArtifact(agentCtx("a"), &pb.UploadArtifactRequest{Content: make([]byte, 65)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for an oversized artifact, got %v", err)
	}
	if len(raw.rows) != 0 {
		t.Fatalf("an oversized artifact was recorded")
	}
}

func TestGetArtifact_ScopeDeniedReportsNotFound(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(map[string]*domain.TagPredicate{
		"support": {ForbiddenTags: []string{"secrets"}},
	}, nil, []string{"secrets"})
	raw.rows["led_h0"] = domain.LedgerEntry{ID: "led_h0", Kind: domain.LedgerArtifact, Tags: []string{"secrets"}}

	resp, err := s.GetArtifact(agentCtx("support"), &pb.GetArtifactRequest{Hash: "led_h0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Found {
		t.Errorf("a denied artifact must report found=false (no existence leak)")
	}
}

// GetArtifact serves artifacts only: a readable tool result is not an artifact.
func TestGetArtifact_NonArtifactEntryIsNotFound(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(map[string]*domain.TagPredicate{"a": {}}, nil, nil)
	raw.rows["led_tr"] = domain.LedgerEntry{ID: "led_tr", Kind: domain.LedgerToolResult, Body: []byte("x")}
	if resp, err := s.GetArtifact(agentCtx("a"), &pb.GetArtifactRequest{Hash: "led_tr"}); err != nil || resp.Found {
		t.Fatalf("a tool result was served as an artifact: %+v %v", resp, err)
	}
	if resp, err := s.GetArtifact(agentCtx("a"), &pb.GetArtifactRequest{Hash: "deadbeef"}); err != nil || resp.Found {
		t.Fatalf("a non-ledger reference resolved: %+v %v", resp, err)
	}
}

func TestListStepArtifacts_FiltersByScope(t *testing.T) {
	t.Parallel()
	s, raw := newArtifactServer(map[string]*domain.TagPredicate{
		"support": {ForbiddenTags: []string{"secrets"}},
	}, nil, []string{"secrets", "public_kb"})
	raw.rows["led_h0"] = domain.LedgerEntry{ID: "led_h0", Kind: domain.LedgerArtifact, SessionID: "s", StepIndex: 1, Tags: []string{"public_kb"}}
	raw.rows["led_h1"] = domain.LedgerEntry{ID: "led_h1", Kind: domain.LedgerArtifact, SessionID: "s", StepIndex: 1, Tags: []string{"secrets"}}

	resp, err := s.ListStepArtifacts(agentCtx("support"), &pb.ListStepArtifactsRequest{SessionId: "s", StepIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Hash != "led_h0" {
		t.Errorf("support agent must see only the public artifact, got %+v", resp.Artifacts)
	}
}

// With the ledger off the artifact plane answers Unimplemented (no second store).
func TestArtifactPlane_LedgerOffIsUnimplemented(t *testing.T) {
	t.Parallel()
	s := &Server{}
	if _, err := s.UploadArtifact(agentCtx("a"), &pb.UploadArtifactRequest{Content: []byte("x")}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("UploadArtifact with the ledger off: %v", err)
	}
	if _, err := s.GetArtifact(agentCtx("a"), &pb.GetArtifactRequest{Hash: "led_x"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetArtifact with the ledger off: %v", err)
	}
}
