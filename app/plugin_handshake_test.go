package app

import (
	"testing"
	"time"
)

// ADR-0089: the composition root maps composed plugin statuses into the operator
// plane's shape. The mapping lives here so the operator package never learns how
// plugins are composed — the same separation that keeps the kernel from
// interpreting a capability string (ADR-0082 D2).
func TestPluginHandshake_CarriesIdentityAndPanels(t *testing.T) {
	expiry := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	out := pluginHandshake([]PluginStatus{{
		Manifest: PluginManifest{
			ID:           "authz",
			DisplayName:  "Access Policy",
			Version:      "1.0.0",
			Capabilities: []string{"access-policy"},
			Panels: []PanelSpec{
				{ID: "access-policy", Title: "Access Policy", Capability: "access-policy"},
			},
		},
		State:     PluginStateActive,
		ExpiresAt: &expiry,
	}})

	if len(out) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(out))
	}
	got := out[0]
	if got.ID != "authz" || got.DisplayName != "Access Policy" || got.Version != "1.0.0" {
		t.Errorf("identity drifted: %+v", got)
	}
	if got.State != PluginStateActive {
		t.Errorf("state drifted: %q", got.State)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "access-policy" {
		t.Errorf("capabilities drifted: %v", got.Capabilities)
	}
	if len(got.Panels) != 1 || got.Panels[0].Capability != "access-policy" {
		t.Errorf("panels drifted: %+v", got.Panels)
	}
	// An expiry is only useful to a console if it is machine-readable.
	if got.ExpiresAt != "2026-08-01T12:00:00Z" {
		t.Errorf("expiry must be RFC3339, got %q", got.ExpiresAt)
	}
}

// A plugin with no expiry reports an empty string, not a zero timestamp — "no
// expiry" and "expired at the beginning of time" are different facts.
func TestPluginHandshake_NoExpiryIsEmpty(t *testing.T) {
	out := pluginHandshake([]PluginStatus{{
		Manifest: PluginManifest{ID: "reactive", Version: "1.0.0"},
		State:    PluginStateActive,
	}})
	if len(out) != 1 || out[0].ExpiresAt != "" {
		t.Fatalf("expected an empty expiry, got %+v", out)
	}
}

// A plugin that did NOT register still reaches the console, carrying why. That
// distinction is the reason the field exists: a console cannot otherwise tell
// "this deployment has no such plugin" from "it declined to register".
func TestPluginHandshake_ReportsWhyAPluginIsAbsent(t *testing.T) {
	out := pluginHandshake([]PluginStatus{
		{Manifest: PluginManifest{ID: "reactive"}, State: PluginStateDepsUnmet, Missing: []string{"authz"}},
		{Manifest: PluginManifest{ID: "langfuse"}, State: PluginStateNotEntitled, Reason: "not in package"},
	})
	if len(out) != 2 {
		t.Fatalf("every declared plugin must be reported, got %d", len(out))
	}
	if out[0].State != PluginStateDepsUnmet || len(out[0].Missing) != 1 || out[0].Missing[0] != "authz" {
		t.Errorf("unmet dependencies dropped: %+v", out[0])
	}
	if out[1].State != PluginStateNotEntitled || out[1].Reason != "not in package" {
		t.Errorf("entitlement reason dropped: %+v", out[1])
	}
}

// An OSS kernel declares no plugins, so the handshake carries an empty list —
// which is what stops a console inventing a warning for a correct deployment.
func TestPluginHandshake_OSSIsEmpty(t *testing.T) {
	if got := pluginHandshake(nil); len(got) != 0 {
		t.Errorf("expected no plugins, got %+v", got)
	}
}
