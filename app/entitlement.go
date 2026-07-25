package app

import "time"

// EntitlementProvider decides which plugins this deployment may activate (ADR-0082 D3).
//
// It is consulted at the SINGLE chokepoint in applyPlugins, before a plugin's Register is
// called — so a plugin that is not entitled contributes literally nothing: no capability
// strings, no gRPC services, no lifecycles, no goroutines, no operator surface. Its cost is
// dormant code on disk and nothing else.
//
// That is the governing principle of the licensing design: **"not entitled" behaves exactly
// like "not installed."** Rather than a parallel system of paid-feature switches, unpaid and
// unbuilt follow the same path — the one the OSS build exercises on every boot, and which is
// therefore already tested.
//
// Enforcement lives HERE, never in the UI. Hiding a panel is cosmetic; anyone can call the
// gRPC surface directly. Because an unentitled plugin never registered, its services do not
// exist to be called and its RPCs answer Unimplemented by construction.
//
// Tier-3, never pluggable (ADR-0074): a plugin must never be able to install or replace the
// entitlement provider — that would be a self-granting privilege escalation.
type EntitlementProvider interface {
	// Entitled reports whether the plugin may activate. Returning an error is treated as
	// NOT entitled (fail-closed) — a licensing check that cannot answer must not
	// accidentally grant. Offline resilience is provided by the licence's grace period
	// (ADR-0082 D5), not by failing open here.
	Entitled(m PluginManifest) (Entitlement, error)
}

// Entitlement is the provider's verdict for one plugin.
type Entitlement struct {
	// Allowed gates registration. False ⇒ the plugin contributes nothing.
	Allowed bool
	// State is the operator-facing status: PluginStateActive, PluginStateNotEntitled, or
	// PluginStateExpired. Empty defaults to active/not_entitled from Allowed.
	State string
	// Reason is operator-facing detail ("licence expired 2026-06-01; grace ends 2026-06-15").
	Reason string
	// ExpiresAt, when set, is surfaced so a UI can warn ahead of a lapse.
	ExpiresAt *time.Time
}

// resolveEntitlement applies the provider to one manifest, normalising the verdict. A nil
// provider allows everything — the OSS default, since the OSS build ships no paid plugins.
func resolveEntitlement(p EntitlementProvider, m PluginManifest) Entitlement {
	if p == nil {
		return Entitlement{Allowed: true, State: PluginStateActive}
	}
	e, err := p.Entitled(m)
	if err != nil {
		// Fail closed, but say why — a silently disabled paid feature is a support nightmare.
		return Entitlement{
			Allowed: false,
			State:   PluginStateNotEntitled,
			Reason:  "entitlement check failed: " + err.Error(),
		}
	}
	if e.State == "" {
		if e.Allowed {
			e.State = PluginStateActive
		} else {
			e.State = PluginStateNotEntitled
		}
	}
	return e
}
