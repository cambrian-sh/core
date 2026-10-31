package domain

import (
	"context"
	"strings"
	"testing"
)

// The CL-4 gate (ADR-0127, owner ruling OQ-G 2026-10-31): an agent holding the
// opt-in fleet grant `local:*` lists and calls its served owner's machine tools
// with no per-tool grant; an agent without it sees and gets nothing; and the
// fleet grant never reaches another owner's machine — ownership is still decided
// at resolution, before any grant is consulted.

func TestGrantCovers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		grant, tool string
		want        bool
	}{
		{"kernel_tool", "kernel_tool", true},
		{"kernel_tool", "other_tool", false},
		{LocalFleetGrant, "local:m/read_file", true},
		{LocalFleetGrant, "kernel_tool", false},      // the fleet grant is not allow-all
		{LocalFleetGrant, "local:m/", false},         // malformed contributed names
		{LocalFleetGrant, "local:/read_file", false}, // never match the wildcard
		{LocalFleetGrant, "local:", false},
		{"local:m/read_file", "local:m/read_file", true}, // explicit grants still work
		{"local:m/read_file", "local:m/write_file", false},
		{"local:m/*", "local:m/read_file", false}, // no other wildcard exists
	}
	for _, c := range cases {
		if got := grantCovers(ToolGrant{Tool: c.grant}, c.tool); got != c.want {
			t.Errorf("grantCovers(%q, %q) = %v, want %v", c.grant, c.tool, got, c.want)
		}
	}
}

// twoOwnerFleet registers owner A's and owner B's machines, both live.
func twoOwnerFleet(t *testing.T) *InMemoryFleet {
	t.Helper()
	f := ownerFleet(t, true)
	if err := f.RegisterWorker(WorkerRegistration{
		Machine: "b-machine",
		Owner:   AgentPrincipal("owner-b"),
		Tools: []SystemTool{
			{Name: "read_file", Description: "Read one file.", Effects: []ToolEffect{EffectRead}},
		},
		Consent: ConsentAuto,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	f.SetLive("b-machine", true)
	return f
}

func fleetGrantExec(t *testing.T) *ToolExecutor {
	t.Helper()
	e := contributionExec(twoOwnerFleet(t))
	e.Unrestricted = false
	e.Grants = grantStore("fleet-agent", ToolGrant{Tool: LocalFleetGrant, Policy: ToolResourcePolicy{AllowAll: true}})
	return e
}

func TestFleetGrant_HolderListsAndCallsOwnFleet(t *testing.T) {
	t.Parallel()
	e := fleetGrantExec(t)
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	menu := menuNames(e.AvailableTools(ctx, "fleet-agent"))
	if !menu["local:a-machine/read_file"] {
		t.Fatalf("fleet-grant holder's menu lacks its owner's tool: %v", menu)
	}
	if menu["local:b-machine/read_file"] {
		t.Fatalf("fleet-grant holder's menu lists another owner's machine: %v", menu)
	}
	if menu["kernel_tool"] {
		t.Errorf("the fleet grant leaked a kernel tool into the menu: %v", menu)
	}

	resp := e.Execute(ctx, ToolCallRequest{AgentID: "fleet-agent", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if resp.Denied {
		t.Fatalf("fleet-grant holder was refused its owner's tool: %+v", resp)
	}
}

func TestFleetGrant_NonHolderSeesAndGetsNothing(t *testing.T) {
	t.Parallel()
	e := fleetGrantExec(t)
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	for name := range menuNames(e.AvailableTools(ctx, "plain-agent")) {
		if strings.HasPrefix(name, LocalToolPrefix) {
			t.Errorf("agent without the fleet grant sees contributed tool %q", name)
		}
	}
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "plain-agent", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if !resp.Denied || !strings.Contains(resp.DenyReason, "not granted") {
		t.Fatalf("agent without the fleet grant was not refused at the grant gate: %+v", resp)
	}
}

// Ownership is decided before the grant: holding `local:*` while serving owner A
// must not reach owner B's machine.
func TestFleetGrant_NeverReachesAnotherOwnersMachine(t *testing.T) {
	t.Parallel()
	e := fleetGrantExec(t)
	var recorded []AccessDecision
	e.FleetDecisions = func(d AccessDecision) { recorded = append(recorded, d) }
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	resp := e.Execute(ctx, ToolCallRequest{AgentID: "fleet-agent", ToolName: "local:b-machine/read_file", ArgsJSON: []byte(`{}`)})
	if !resp.Denied {
		t.Fatalf("fleet grant reached another owner's machine: %+v", resp)
	}
	if len(recorded) != 1 || recorded[0].Reason != ReasonWorkerNotOwned {
		t.Fatalf("cross-owner refusal not recorded as %s: %+v", ReasonWorkerNotOwned, recorded)
	}
}

// The menu and the gate answer through one matcher: for every contributed tool
// the gate would allow, the menu offers it, and vice versa.
func TestFleetGrant_MenuMatchesGate(t *testing.T) {
	t.Parallel()
	e := fleetGrantExec(t)
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))
	for _, agent := range []string{"fleet-agent", "plain-agent"} {
		menu := menuNames(e.AvailableTools(ctx, agent))
		for _, tool := range []string{"local:a-machine/read_file"} {
			_, gate := e.grantFor(ctx, agent, tool, "")
			if menu[tool] != gate {
				t.Errorf("agent %s, tool %s: menu offers=%v, gate allows=%v", agent, tool, menu[tool], gate)
			}
		}
	}
}

// Receipts and audits name the concrete tool, never the wildcard.
func TestFleetGrant_ReportsConcreteTool(t *testing.T) {
	t.Parallel()
	e := fleetGrantExec(t)
	g, ok := e.grantFor(context.Background(), "fleet-agent", "local:a-machine/read_file", "")
	if !ok {
		t.Fatal("fleet grant did not cover a well-formed contributed tool")
	}
	if g.Tool != "local:a-machine/read_file" || !g.Policy.AllowAll {
		t.Errorf("grant = %+v, want the concrete tool with the fleet grant's policy", g)
	}
}
