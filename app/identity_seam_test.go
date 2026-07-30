package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// stubIdentityResolver records what the inbound path asked it.
type stubIdentityResolver struct {
	binding domain.IdentityBinding
	bound   bool
	policy  domain.StrangerPolicy
	asked   []string
}

func (s *stubIdentityResolver) ResolveIdentity(_ context.Context, _ string, p domain.SenderProfile) (domain.IdentityBinding, bool) {
	s.asked = append(s.asked, p.ExternalID)
	return s.binding, s.bound
}

func (s *stubIdentityResolver) StrangerPolicyFor(context.Context, string) domain.StrangerPolicy {
	return s.policy
}

// identityPlugin claims the identity seam at Register time.
type identityPlugin struct {
	name string
	res  domain.IdentityResolver
}

func (p *identityPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: p.name, DisplayName: p.name, Version: "test"}
}

func (p *identityPlugin) Register(r *Registry) error {
	return r.SetIdentityResolver(p.name, p.res)
}

// TestIdentityResolver_ReachesOptions is the regression that matters: the
// resolver was implemented, the RPCs were served, and nothing ever installed it
// — so every sender stayed the ingress daemon's principal, the unbound worklist
// was permanently empty, and blocking a sender did nothing to the path that
// would have carried them.
func TestIdentityResolver_ReachesOptions(t *testing.T) {
	res := &stubIdentityResolver{}

	composed, err := applyPlugins(Options{
		Plugins: []Plugin{&identityPlugin{name: "authz", res: res}},
	})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}

	if composed.opts.IdentityResolver == nil {
		t.Fatal("a registered identity resolver never reached Options; the inbound path would resolve nobody")
	}
	if composed.opts.IdentityResolver != domain.IdentityResolver(res) {
		t.Fatal("a different resolver reached Options than the one the plugin registered")
	}
}

// TestIdentityResolver_SingleOwner guards the tier-1 replace-one rule. Two
// registries could disagree about who an external id maps to, and a binding
// decides REACH — there would be no way to say which answer held, and the
// safe-looking one is not necessarily the one that ran.
func TestIdentityResolver_SingleOwner(t *testing.T) {
	_, err := applyPlugins(Options{
		Plugins: []Plugin{
			&identityPlugin{name: "authz", res: &stubIdentityResolver{}},
			&identityPlugin{name: "impostor", res: &stubIdentityResolver{}},
		},
	})
	if err == nil {
		t.Fatal("a second plugin must not be able to take ownership of identity resolution")
	}
}

// TestIdentityResolver_ExplicitOptionWins keeps the precedence every other seam
// uses: a composition root that passed its own resolver is not overridden.
func TestIdentityResolver_ExplicitOptionWins(t *testing.T) {
	explicit := &stubIdentityResolver{}

	composed, err := applyPlugins(Options{
		IdentityResolver: explicit,
		Plugins:          []Plugin{&identityPlugin{name: "authz", res: &stubIdentityResolver{}}},
	})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if composed.opts.IdentityResolver != domain.IdentityResolver(explicit) {
		t.Fatal("an explicitly configured resolver must not be replaced by a plugin's")
	}
}

// TestIdentityResolver_AbsentIsValid: no plugin claims the seam, and that is the
// pre-0077 behaviour rather than a failure — the surface IS the identity.
func TestIdentityResolver_AbsentIsValid(t *testing.T) {
	composed, err := applyPlugins(Options{Plugins: []Plugin{&testPlugin{name: "noop"}}})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if composed.opts.IdentityResolver != nil {
		t.Fatal("nothing registered a resolver, so none should be installed")
	}
}
