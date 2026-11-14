package config

import (
	"path/filepath"
	"slices"
	"testing"
)

// ENG-668: the bundled-Keycloak compose overlay
// (cambrian-premium/deploy/keycloak/docker-compose.keycloak.yml) configures the
// kernel purely through these CAMBRIAN_IDENTITY__OIDC__* variables. This proves
// the names land on identity.oidc — including the realm-role → Cambrian-role map,
// whose keys come from env as single strings — and that the result validates.
func keycloakTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("CAMBRIAN_DATABASE__HOST", "cambrian-db")
	t.Setenv("CAMBRIAN_DATABASE__USER", "u")
	t.Setenv("CAMBRIAN_DATABASE__PASSWORD", "p")
}

func TestKeycloakOverlayEnvConfiguresOIDC(t *testing.T) {
	keycloakTestDB(t)
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ISSUER", "https://keycloak.localhost:8443/realms/cambrian")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__CLIENT_ID", "cambrian-studio")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ROLES_CLAIM", "roles")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ROLE_MAP__CAMBRIAN_OPERATOR", "operator")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ROLE_MAP__CAMBRIAN_VIEWER", "viewer")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__FIRST_LOGIN", "require_provisioned")
	t.Setenv("CAMBRIAN_SERVER__INSECURE_LOCALHOST", "false")

	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	o := cfg.Identity.OIDC
	if !o.Enabled() || o.ClientID != "cambrian-studio" || o.RolesClaim != "roles" || o.FirstLogin != "require_provisioned" {
		t.Fatalf("oidc = %+v", o)
	}
	if !slices.Equal(o.RoleMap["cambrian_operator"], []string{"operator"}) ||
		!slices.Equal(o.RoleMap["cambrian_viewer"], []string{"viewer"}) {
		t.Fatalf("role_map = %v; the realm roles must map to operator/viewer", o.RoleMap)
	}
}

// With the overlay applied but CAMBRIAN_OIDC_ISSUER unset, compose passes an empty
// issuer: OIDC stays off and the role map alone changes nothing.
func TestKeycloakOverlayEmptyIssuerLeavesOIDCOff(t *testing.T) {
	keycloakTestDB(t)
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ISSUER", "")
	t.Setenv("CAMBRIAN_IDENTITY__OIDC__ROLE_MAP__CAMBRIAN_OPERATOR", "operator")
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Identity.OIDC.Enabled() {
		t.Fatal("an empty issuer must leave OIDC off")
	}
}
