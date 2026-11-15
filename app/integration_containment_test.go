package app

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// T-20261106-integrationop-has-no-structured-containment: a stdio server's
// ADR-0139 D8 containment reaches the console as a structured field on both
// IntegrationOp and (through the real operator service) MCPServerOp, carrying
// the same facts as the policy_summary text older consoles read; an http
// server, which the kernel does not launch, has none.
func TestIntegrationContainment_StructuredOnBothOps(t *testing.T) {
	srv := newAuthServer(t)
	r := newIntegrationRig(t, srv.url)
	off := false
	r.m.runtime.upsertConfig(config.MCPServerConfig{ID: "localtool", Transport: "stdio", Endpoint: "node",
		Args: []string{"server.js"}, Enabled: &off}, mcpSourceStore)

	op := r.find("localtool")
	if op == nil {
		t.Fatal("stdio server not listed")
	}
	c := op.GetContainment()
	if c == nil || c.GetTier() == "" || c.GetRunner() == "" || c.GetSummary() == "" {
		t.Fatalf("stdio server has no structured containment: %+v", c)
	}
	if !strings.Contains(op.GetPolicySummary(), c.GetSummary()) {
		t.Fatalf("policy_summary %q must keep the containment text %q for older consoles", op.GetPolicySummary(), c.GetSummary())
	}
	if len(c.GetEnforced())+len(c.GetMissing()) == 0 && !c.GetDisabled() {
		t.Fatalf("containment reports neither enforced nor missing controls: %+v", c)
	}
	if http := r.find("filesrv"); http == nil || http.GetContainment() != nil {
		t.Fatalf("an http server must carry no containment: %+v", http.GetContainment())
	}

	svc := &operator.Service{}
	svc.SetWave1Reads(nil, mcpLister{runtime: r.m.runtime, connector: r.m.connector}, nil, nil)
	resp, err := svc.ListMCPServers(context.Background(), &pb.ListMCPServersOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got *pb.MCPServerOp
	for _, s := range resp.GetServers() {
		if s.GetName() == "localtool" {
			got = s
		}
		if s.GetName() == "filesrv" && s.GetContainment() != nil {
			t.Fatal("ListMCPServers: an http server must carry no containment")
		}
	}
	if got == nil || got.GetContainment().GetSummary() != c.GetSummary() || got.GetContainment().GetTier() != c.GetTier() {
		t.Fatalf("ListMCPServers containment %+v, want the IntegrationOp's %+v", got.GetContainment(), c)
	}
}

// Owner decision 2026-11-15 (T-20261115-console-custom-mcp-server-install-cannot-set):
// the console may set ANY tier on an installed server — lower as well as raise —
// and the launch containment follows it; an unknown tier is refused; clearing
// returns the server to the default tier. The tier before each change is
// returned for the audit.
func TestSetTrustTier_AnyTierReachesTheContainment(t *testing.T) {
	srv := newAuthServer(t)
	r := newIntegrationRig(t, srv.url)
	ctx := context.Background()
	// Installed through the console, disabled (never launched here).
	if out, _, err := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{Id: "localtool", Custom: &pb.IntegrationCustomSpecOp{
		Transport: "stdio", Endpoint: "node", Args: []string{"server.js"},
		HasDefaultPolicy: true, DefaultPolicy: &pb.IntegrationToolPolicyOp{Effects: []string{"read"}},
	}}); err != nil || out.Effect == operator.EffectRejected {
		t.Fatalf("install: %+v %v", out, err)
	}
	defaultTier := r.find("localtool").GetContainment().GetTier()

	for _, tier := range []string{"first-party", "third-party", "vetted"} {
		out, op, before, err := r.m.SetTrustTier(ctx, "localtool", tier)
		if err != nil || out.Effect == operator.EffectRejected {
			t.Fatalf("set %s: %+v %v", tier, out, err)
		}
		if before == "" {
			t.Fatalf("set %s: the tier before the change must be reported for the audit", tier)
		}
		if op.GetTrustTier() != tier || r.find("localtool").GetContainment().GetTier() != tier {
			t.Fatalf("set %s: op tier %q, containment tier %q", tier, op.GetTrustTier(), r.find("localtool").GetContainment().GetTier())
		}
	}
	if out, _, _, _ := r.m.SetTrustTier(ctx, "localtool", "system"); out.Effect != operator.EffectRejected {
		t.Fatalf("an unknown tier must be refused: %+v", out)
	}
	if _, _, before, _ := r.m.SetTrustTier(ctx, "localtool", ""); before != "vetted" {
		t.Fatalf("clearing reports the tier it replaced, got %q", before)
	}
	if got := r.find("localtool").GetContainment().GetTier(); got != defaultTier {
		t.Fatalf("cleared tier: containment %q, want the default %q", got, defaultTier)
	}
}
