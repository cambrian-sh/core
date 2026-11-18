package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2083: an explicit third-party tier wins over system status — it only
// lowers privilege — so the kernel's third-party composed runtime (System, so
// selection never picks it) launches at the third-party sandbox tier, while the
// shared runtime and other system agents keep theirs.
func TestAgentTrustTier_ThirdPartyWinsOverSystem(t *testing.T) {
	cases := []struct {
		def  domain.AgentDefinition
		want string
	}{
		{domain.AgentDefinition{ID: "composed_worker_third_party", System: true, TrustTier: "third-party"}, agentTierThirdParty},
		{domain.AgentDefinition{ID: "composed_worker", System: true, TrustTier: "first-party"}, agentTierSystem},
		{domain.AgentDefinition{ID: "organ", System: true}, agentTierSystem},
		{domain.AgentDefinition{ID: "p", TrustTier: "system"}, agentTierFirstParty},
	}
	for _, c := range cases {
		if got := agentTrustTier(&c.def); got != c.want {
			t.Errorf("%s: tier %q, want %q", c.def.ID, got, c.want)
		}
	}
	if sandboxTier(&domain.AgentDefinition{System: true, TrustTier: "third-party"}) != sandbox.TierThirdParty {
		t.Error("the third-party runtime must get the third-party sandbox tier")
	}
}
