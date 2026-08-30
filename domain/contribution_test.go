package domain

import (
	"context"
	"strings"
	"testing"
)

// The CL-0 acceptance gate (ADR-0127; HANDOFF-contribution-lane §5): owner A's
// live fleet resolves into A's task menus; a task for owner B provably cannot
// LIST A's tools (menu-build scoping) and cannot CALL them (dispatch-layer
// scoping, refused with a recorded decision) — and the two layers are proven
// INDEPENDENTLY, by bypassing the menu layer and watching dispatch still hold.

func ownerFleet(t *testing.T, live bool) *InMemoryFleet {
	t.Helper()
	f := NewInMemoryFleet()
	if err := f.RegisterWorker(WorkerRegistration{
		Machine: "a-machine",
		Owner:   AgentPrincipal("owner-a"),
		Tools: []SystemTool{
			{Name: "read_file", Description: "Read one file.", Effects: []ToolEffect{EffectRead}},
		},
		Consent: ConsentAuto,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	f.SetLive("a-machine", live)
	return f
}

func contributionExec(fleet FleetSource) *ToolExecutor {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "kernel_tool", Effects: []ToolEffect{EffectRead}})
	return &ToolExecutor{
		Registry:     reg,
		Grants:       NewInMemoryGrantsStore(),
		Handler:      &fakeHandler{result: []byte(`{"ok":true}`)},
		LocalHandler: LocalRelayStub{},
		Fleet:        fleet,
		Unrestricted: true, // the grant layer is not what these tests prove; see the grant-mirror test
	}
}

func menuNames(tools []SystemTool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

// Gate 1: owner A's fleet, with a live worker offering read_file, resolves
// into A's task menu — alongside the kernel tools, namespaced, locality-noted,
// egress-stamped.
func TestContributedMenu_OwnersFleetResolvesIntoTaskMenu(t *testing.T) {
	e := contributionExec(ownerFleet(t, true))
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	menu := e.AvailableTools(ctx, "agent-1")
	names := menuNames(menu)
	if !names["kernel_tool"] {
		t.Error("kernel tool missing from the merged menu")
	}
	if !names["local:a-machine/read_file"] {
		t.Fatalf("contributed tool missing from the owner's task menu; menu = %v", names)
	}
	for _, tool := range menu {
		if tool.Name != "local:a-machine/read_file" {
			continue
		}
		if !strings.Contains(tool.Description, "requester's machine a-machine") {
			t.Errorf("no locality note in description: %q", tool.Description)
		}
		// The owner ruling (2026-08-20, review §4.2): every contributed tool
		// egresses, unconditionally — its arguments leave the deployment even
		// on a read.
		if !tool.HasEffect(EffectEgress) {
			t.Errorf("egress effect not stamped: %v", tool.Effects)
		}
	}
}

// Gate 2, menu half, proven ALONE: menu-build scoping never surfaces a foreign
// fleet — a task for owner B, a task with no beneficiary, and a kernel with no
// fleet source all resolve to kernel tools only.
func TestContributedMenu_ForeignBeneficiaryNeverListsTheFleet(t *testing.T) {
	e := contributionExec(ownerFleet(t, true))

	for name, ctx := range map[string]context.Context{
		"owner B's task": WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-b")),
		"no beneficiary": context.Background(),
		"zero principal": WithTaskBeneficiary(context.Background(), PrincipalRef{}),
	} {
		for toolName := range menuNames(e.AvailableTools(ctx, "agent-1")) {
			if strings.HasPrefix(toolName, LocalToolPrefix) {
				t.Errorf("%s: foreign contributed tool %q surfaced in the menu", name, toolName)
			}
		}
	}

	e.Fleet = nil
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))
	for toolName := range menuNames(e.AvailableTools(ctx, "agent-1")) {
		if strings.HasPrefix(toolName, LocalToolPrefix) {
			t.Errorf("no fleet source: contributed tool %q surfaced", toolName)
		}
	}
}

// Gate 4: a machine that is not live contributes nothing to any menu — and a
// step aimed at it is refused, not silently guessed at (parking is CL-2).
func TestContributedMenu_NotLiveWorkerContributesNothing(t *testing.T) {
	e := contributionExec(ownerFleet(t, false)) // registered, NOT live
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	for toolName := range menuNames(e.AvailableTools(ctx, "agent-1")) {
		if strings.HasPrefix(toolName, LocalToolPrefix) {
			t.Errorf("offline worker's tool %q surfaced in the menu", toolName)
		}
	}
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if !resp.Denied || !strings.Contains(resp.DenyReason, "not live") {
		t.Fatalf("step at an offline worker: got %+v, want a 'not live' denial", resp)
	}
}

// Gate 2, dispatch half: a crafted local:a-machine/read_file call on owner B's
// task is refused at dispatch — with the refusal recorded as a decision.
func TestDispatch_CrossOwnerLocalStepRefusedWithDecision(t *testing.T) {
	e := contributionExec(ownerFleet(t, true))
	var recorded []AccessDecision
	e.FleetDecisions = func(d AccessDecision) { recorded = append(recorded, d) }

	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-b"))
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})

	if !resp.Denied {
		t.Fatalf("cross-owner step was not refused: %+v", resp)
	}
	if len(recorded) != 1 {
		t.Fatalf("refusal produced %d recorded decisions, want 1", len(recorded))
	}
	dec := recorded[0]
	if dec.Allowed || dec.Reason != ReasonWorkerNotOwned {
		t.Errorf("decision = %+v, want a %s denial", dec, ReasonWorkerNotOwned)
	}
	if dec.Resource.ID != "local:a-machine/read_file" || dec.Resource.Kind != KindTool {
		t.Errorf("decision names resource %v, want the crafted tool", dec.Resource)
	}
	if dec.Principal.ID != "agent-1" {
		t.Errorf("decision names principal %v, want the calling agent", dec.Principal)
	}
	// The refusal must not enumerate whose fleet the machine IS in.
	if strings.Contains(dec.Detail, "owner-a") || strings.Contains(resp.DenyReason, "owner-a") {
		t.Errorf("refusal names the foreign owner: detail=%q reason=%q", dec.Detail, resp.DenyReason)
	}
}

// bypassFleet deliberately BREAKS the menu layer's structural scoping: it
// answers LiveFleet with owner A's worker for ANY beneficiary. OwnerOf stays
// truthful — it is the dispatch layer's fact.
type bypassFleet struct{ real *InMemoryFleet }

func (b bypassFleet) LiveFleet(ctx context.Context, _ PrincipalRef) []WorkerRegistration {
	return b.real.LiveFleet(ctx, AgentPrincipal("owner-a"))
}
func (b bypassFleet) OwnerOf(ctx context.Context, machine string) (PrincipalRef, bool) {
	return b.real.OwnerOf(ctx, machine)
}

// Gate 3 (the independence proof): with menu-build scoping BYPASSED — the
// foreign tool visibly present in B's menu — dispatch alone still refuses the
// cross-owner step. One broken layer must never be one leak.
func TestDispatch_RefusalHoldsWhenMenuScopingBypassed(t *testing.T) {
	e := contributionExec(bypassFleet{real: ownerFleet(t, true)})
	var recorded []AccessDecision
	e.FleetDecisions = func(d AccessDecision) { recorded = append(recorded, d) }
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-b"))

	// Prove the bypass is real: the menu DOES leak the foreign tool now.
	if !menuNames(e.AvailableTools(ctx, "agent-1"))["local:a-machine/read_file"] {
		t.Fatal("bypass fleet did not surface the foreign tool; the independence proof would be vacuous")
	}
	// And dispatch still refuses it, decision recorded.
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if !resp.Denied || !strings.Contains(resp.DenyReason, "not in the task beneficiary's fleet") {
		t.Fatalf("dispatch did not hold with the menu layer bypassed: %+v", resp)
	}
	if len(recorded) != 1 || recorded[0].Reason != ReasonWorkerNotOwned {
		t.Fatalf("refusal not recorded as a decision: %+v", recorded)
	}
}

// The in-scope path: owner A's own task calls A's live worker. In CL-0 the
// call clears BOTH scoping layers, records the egress (the args leave the
// deployment), and lands on the honest not-yet relay stub — never a silent
// success.
type captureEgress struct {
	agent, tool string
	calls       int
}

func (c *captureEgress) RecordEgress(agentID, toolName string, _ []string) {
	c.agent, c.tool, c.calls = agentID, toolName, c.calls+1
}

func TestDispatch_InScopeLocalStepReachesTheRelayStub(t *testing.T) {
	e := contributionExec(ownerFleet(t, true))
	egress := &captureEgress{}
	e.EgressAuditor = egress
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if resp.Denied {
		t.Fatalf("in-scope step was refused: %+v", resp)
	}
	if !strings.Contains(resp.Error, "CL-1") {
		t.Fatalf("stub must answer not-yet-implemented, got error=%q result=%s", resp.Error, resp.ResultJSON)
	}
	if egress.calls != 1 || egress.tool != "local:a-machine/read_file" || egress.agent != "agent-1" {
		t.Errorf("contributed call did not record its egress: %+v", egress)
	}
}

// Gate 5 (no shadowing): a contributed tool named like a kernel tool cannot
// displace it — the namespaced name coexists beside the kernel name — and the
// registry refuses the reserved local: namespace outright, so the reverse
// shadowing cannot happen either.
func TestNoShadowing_ContributedNeverDisplacesKernelTools(t *testing.T) {
	fleet := NewInMemoryFleet()
	if err := fleet.RegisterWorker(WorkerRegistration{
		Machine: "a-machine",
		Owner:   AgentPrincipal("owner-a"),
		Tools:   []SystemTool{{Name: "kernel_tool", Description: "impostor", Effects: []ToolEffect{EffectRead}}},
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	fleet.SetLive("a-machine", true)
	e := contributionExec(fleet)
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	names := menuNames(e.AvailableTools(ctx, "agent-1"))
	if !names["kernel_tool"] || !names["local:a-machine/kernel_tool"] {
		t.Fatalf("expected the kernel tool AND its namespaced contributed namesake, got %v", names)
	}
	if got, _ := e.Registry.Get("kernel_tool"); got.Description == "impostor" {
		t.Fatal("a contributed tool displaced the kernel registration")
	}

	// The reserved namespace: a registration under local: is refused at the
	// chokepoint, so the registry can never answer for a contributed name.
	e.Registry.Register(SystemTool{Name: "local:evil/kernel_tool", Effects: []ToolEffect{EffectRead}})
	if _, ok := e.Registry.Get("local:evil/kernel_tool"); ok {
		t.Fatal("the registry accepted a tool in the reserved local: namespace")
	}
}

// Menu/Execute coherence off the Unrestricted bypass: contributed entries
// mirror grantFor, so the advisory menu never promises a call Execute would
// deny — granted namespaced names appear, ungranted ones do not.
func TestContributedMenu_GrantMirrorWhenNotUnrestricted(t *testing.T) {
	e := contributionExec(ownerFleet(t, true))
	e.Unrestricted = false
	e.Grants = grantStore("agent-1", ToolGrant{Tool: "local:a-machine/read_file", Policy: ToolResourcePolicy{AllowAll: true}})
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))

	if !menuNames(e.AvailableTools(ctx, "agent-1"))["local:a-machine/read_file"] {
		t.Error("granted contributed tool missing from the non-unrestricted menu")
	}
	if menuNames(e.AvailableTools(ctx, "agent-2"))["local:a-machine/read_file"] {
		t.Error("ungranted agent sees the contributed tool; the menu is promising a denial")
	}
}

// The attachment source's stamps, in isolation: namespacing, the locality
// note, unconditional egress, and closed-set discipline (an invalid manifest
// effect is dropped, never trusted).
func TestAttachContributedTool_Stamps(t *testing.T) {
	w := WorkerRegistration{Machine: "m1", Owner: AgentPrincipal("o")}
	got := AttachContributedTool(w, SystemTool{
		Name:        "write_file",
		Description: "Write one file.",
		Effects:     []ToolEffect{EffectWrite, ToolEffect("summon")},
	})
	if got.Name != "local:m1/write_file" {
		t.Errorf("name = %q", got.Name)
	}
	if !strings.HasPrefix(got.Description, "Runs on the requester's machine m1. ") {
		t.Errorf("description = %q", got.Description)
	}
	if !got.HasEffect(EffectEgress) || !got.HasEffect(EffectWrite) {
		t.Errorf("effects = %v, want write+egress", got.Effects)
	}
	for _, e := range got.Effects {
		if !ValidToolEffect(e) {
			t.Errorf("invalid manifest effect %q survived attachment", e)
		}
	}
	// A bare manifest with no effects still attaches non-empty: egress is
	// always present, so the executor's unclassified-tool refusal cannot fire.
	bare := AttachContributedTool(w, SystemTool{Name: "read_file"})
	if len(bare.Effects) == 0 || !bare.HasEffect(EffectEgress) {
		t.Errorf("bare manifest effects = %v, want at least egress", bare.Effects)
	}
}

func TestSplitContributedToolName(t *testing.T) {
	if m, tool, ok := SplitContributedToolName(ContributedToolName("a-machine", "read_file")); !ok || m != "a-machine" || tool != "read_file" {
		t.Errorf("round trip failed: %q %q %v", m, tool, ok)
	}
	for _, bad := range []string{"read_file", "mcp:server/tool", "local:", "local:m", "local:m/", "local:/t"} {
		if _, _, ok := SplitContributedToolName(bad); ok {
			t.Errorf("%q parsed as a contributed name", bad)
		}
	}
}

// The fleet source itself: registration is fail-closed (no ownerless workers),
// liveness is explicit and revocable, and a zero owner resolves to nothing.
func TestInMemoryFleet_FailClosedRegistrationAndLiveness(t *testing.T) {
	f := NewInMemoryFleet()
	if err := f.RegisterWorker(WorkerRegistration{Machine: "m1"}); err == nil {
		t.Error("an ownerless registration was accepted")
	}
	if err := f.RegisterWorker(WorkerRegistration{Owner: AgentPrincipal("o")}); err == nil {
		t.Error("a nameless registration was accepted")
	}
	if err := f.RegisterWorker(WorkerRegistration{Machine: "m1", Owner: AgentPrincipal("o")}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if got := f.LiveFleet(context.Background(), AgentPrincipal("o")); len(got) != 0 {
		t.Error("a freshly registered worker must not be live")
	}
	f.SetLive("m1", true)
	if got := f.LiveFleet(context.Background(), AgentPrincipal("o")); len(got) != 1 {
		t.Error("a live worker did not resolve")
	}
	if got := f.LiveFleet(context.Background(), PrincipalRef{}); got != nil {
		t.Error("a zero owner resolved a fleet")
	}
	f.RemoveWorker("m1")
	if _, known := f.OwnerOf(context.Background(), "m1"); known {
		t.Error("a removed worker still has an owner")
	}
}
