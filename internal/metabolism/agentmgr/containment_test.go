package agentmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-716 (owner decision 2026-11-14): first-party launches are unchanged; a
// third-party agent is refused by default (fail closed, the zero-value
// containment) and, under degrade, runs with the tier memory cap.
func TestAdmitLaunch_ByTrustTier(t *testing.T) {
	im := NewInstanceManager("python", "")
	im.SetAgentMemoryLimitMB(1024)

	first := &domain.AgentDefinition{ID: "worker", TrustTier: "first-party"}
	if mb, err := im.admitLaunch(context.Background(), first); err != nil || mb != 1024 {
		t.Fatalf("first-party: mb=%d err=%v, want the unchanged SEC-01 cap", mb, err)
	}
	undeclared := &domain.AgentDefinition{ID: "legacy"}
	if _, err := im.admitLaunch(context.Background(), undeclared); err != nil {
		t.Fatalf("an undeclared local agent is first-party: %v", err)
	}
	claimsSystem := &domain.AgentDefinition{ID: "sneaky", TrustTier: "system"}
	if agentTrustTier(claimsSystem) != agentTierFirstParty {
		t.Fatalf("a profile claiming system must run as first-party")
	}

	third := &domain.AgentDefinition{ID: "vendor-bot", TrustTier: "third-party"}
	_, err := im.admitLaunch(context.Background(), third)
	if err == nil || !strings.Contains(err.Error(), "third-party") || !strings.Contains(err.Error(), "on_unavailable") {
		t.Fatalf("third-party under the zero-value containment must be refused with the fix named: %v", err)
	}

	im.SetAgentContainment(AgentContainment{ThirdPartyDegrade: true, ThirdPartyMemoryMB: 512})
	if mb, err := im.admitLaunch(context.Background(), third); err != nil || mb != 512 {
		t.Fatalf("degraded third-party: mb=%d err=%v, want the tier cap 512", mb, err)
	}
	// A manifest limit smaller than the tier cap still applies; a larger one
	// cannot lift it.
	im.SetManifestResolver(func(_ context.Context, id string) *domain.AgentManifest {
		return &domain.AgentManifest{MemoryLimitMB: map[string]int{"vendor-bot": 256, "big-bot": 8192}[id]}
	})
	if mb, _ := im.admitLaunch(context.Background(), third); mb != 256 {
		t.Fatalf("smaller manifest cap: %d, want 256", mb)
	}
	big := &domain.AgentDefinition{ID: "big-bot", TrustTier: "third-party"}
	if mb, _ := im.admitLaunch(context.Background(), big); mb != 512 {
		t.Fatalf("larger manifest cap lifted the tier's: %d, want 512", mb)
	}
}

// The refusal happens before anything is spawned: the boot path returns the
// tier error, not a spawn error, for a third-party agent.
func TestBootAgentInstance_RefusesThirdPartyBeforeSpawn(t *testing.T) {
	im := NewInstanceManager("definitely-not-an-interpreter", "")
	_, err := im.bootAgentInstance(context.Background(),
		&domain.AgentDefinition{ID: "vendor-bot", TrustTier: "third-party", Runtime: domain.RuntimePython}, false)
	if err == nil || !strings.Contains(err.Error(), "ADR-0139 D8") {
		t.Fatalf("err = %v, want the D8 refusal", err)
	}
	if im.BootCount() != 1 || len(im.cmds) != 0 {
		t.Fatalf("a process was started: cmds=%d", len(im.cmds))
	}
}

// ENG-1467 item 7: the boot path's ctx reaches the manifest read (it used to be
// context.Background() inside the resolver closure).
func TestAdmitLaunch_ManifestReadUsesTheBootContext(t *testing.T) {
	im := NewInstanceManager("python", "")
	var got context.Context
	im.SetManifestResolver(func(ctx context.Context, _ string) *domain.AgentManifest {
		got = ctx
		return nil
	})
	caller := domain.AgentPrincipal("op")
	if _, err := im.admitLaunch(domain.WithPrincipal(context.Background(), caller), &domain.AgentDefinition{ID: "w"}); err != nil {
		t.Fatal(err)
	}
	if got == nil || domain.PrincipalFromContext(got) != caller {
		t.Fatal("manifest read did not run on the boot ctx")
	}
}
