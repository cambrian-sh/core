package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// T-20261118-agent-artifacts-from-planner-lane-steps-carry: the kernel's own
// dispatch binds the instance to the lease it handed the work under, so an
// instance that presents no lease on any call still files its uploads and its
// out/ in that step's run, session and step.
func TestAgentInstanceDispatched_PlacesLeaselessUploadAndOutInTheStep(t *testing.T) {
	t.Parallel()
	s, gw, raw := newOutputsServer(t)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "probe", RunID: "run-d", StepIndex: 0, SessionID: "sess-d"})
	s.AgentInstanceDispatched("probe", "inst-d", lease)

	ctx := domain.WithPrincipal(domain.WithAgentInstance(agentPlaneCtx("probe", "x-agent-id", "probe"), "inst-d"),
		domain.AgentPrincipal("probe"))
	up, err := s.UploadArtifact(ctx, &pb.UploadArtifactRequest{Content: []byte("report")})
	if err != nil {
		t.Fatal(err)
	}
	if e := raw.rows[up.Hash]; e.RunID != "run-d" || e.SessionID != "sess-d" || e.StepIndex != 0 || e.InstanceID != "inst-d" {
		t.Fatalf("lease-less upload provenance: %+v", e)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CollectAgentOutputs(context.Background(), domain.AgentOutputs{AgentID: "probe", InstanceID: "inst-d", Dir: dir}); n != 1 || err != nil {
		t.Fatalf("collect: %d %v", n, err)
	}
	for _, e := range raw.rows {
		if e.ToolName == agentOutToolName && (e.RunID != "run-d" || e.SessionID != "sess-d" || e.StepIndex != 0) {
			t.Fatalf("out/ provenance: %+v", e)
		}
	}
}

// A lease bound to another agent, an unknown lease and an empty instance bind
// nothing: the instance stays unattributed (step -1).
func TestAgentInstanceDispatched_IgnoresForeignOrUnknownLeases(t *testing.T) {
	t.Parallel()
	s, gw, _ := newOutputsServer(t)
	other := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "someone_else", RunID: "run-x", SessionID: "sess-x"})
	s.AgentInstanceDispatched("probe", "inst-f", other)
	s.AgentInstanceDispatched("probe", "inst-f", "garbage")
	s.AgentInstanceDispatched("probe", "", other)
	if b, ok := s.instanceWork().lastFor("inst-f"); ok {
		t.Fatalf("a foreign/unknown lease bound the instance: %+v", b)
	}
}

// Artifact reads resolve the caller's lease identity before the decision point,
// so a credential-authenticated agent working a turn is not refused no_lease.
func TestArtifactRead_ResolvesTheCallersLease(t *testing.T) {
	t.Parallel()
	s, gw, _ := newOutputsServer(t)
	az := &leaseKeyedAuthorizer{}
	s.Authz = az
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		if b.ConversationID == "conv-of-dana" {
			return "dana"
		}
		return ""
	}
	turn := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "probe", ConversationID: "conv-of-dana"})
	ctx := domain.WithPrincipal(agentPlaneCtx("probe", "x-agent-id", "probe", leaseHeader, string(turn)), domain.AgentPrincipal("probe"))
	if _, err := s.GetArtifact(ctx, &pb.GetArtifactRequest{Hash: "led_missing"}); err != nil {
		t.Fatalf("artifact read refused under a live lease: %v", err)
	}
	if az.denied != 0 || len(az.owners) == 0 || az.owners[0] != "dana" {
		t.Fatalf("decision ctx: denied %d owners %v", az.denied, az.owners)
	}
	noLease := domain.WithPrincipal(agentPlaneCtx("probe", "x-agent-id", "probe"), domain.AgentPrincipal("probe"))
	if _, err := s.GetArtifact(noLease, &pb.GetArtifactRequest{Hash: "led_missing"}); err == nil {
		t.Fatal("a lease-less artifact read was not refused")
	}
}
