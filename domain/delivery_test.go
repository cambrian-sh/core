package domain_test

import (
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The envelope rule (ADR-0090): an address is built from a REGISTRATION plus an
// external id, and the namespace is checked here — at bind time — because once an
// address is stored, delivery resolves it from the conversation rather than from
// anything a caller supplies.
func TestDeliveryFor_EnforcesTheNamespace(t *testing.T) {
	tg := domain.IngressRegistration{AgentID: "telegram_ingress", Namespace: []string{"tg:"}}

	addr, err := tg.DeliveryFor("tg:12345")
	if err != nil {
		t.Fatalf("an id inside the namespace must be accepted: %v", err)
	}
	if addr.IngressAgentID != "telegram_ingress" || addr.ExternalID != "tg:12345" {
		t.Errorf("address drifted: %+v", addr)
	}

	// The impersonation guard: a Telegram bridge must not be able to address a
	// Slack identity, even though it can reach this call.
	if _, err := tg.DeliveryFor("slack:U42"); !errors.Is(err, domain.ErrOutsideNamespace) {
		t.Errorf("expected ErrOutsideNamespace, got %v", err)
	}
	if _, err := tg.DeliveryFor(""); err == nil {
		t.Error("an empty external id must never produce an address")
	}
}

func TestDeliveryFor_UnregisteredIngressCannotAddress(t *testing.T) {
	var none domain.IngressRegistration
	if _, err := none.DeliveryFor("tg:1"); err == nil {
		t.Error("a zero registration must not produce a deliverable address")
	}
}

// An address is only deliverable when BOTH halves are present: an ingress with no
// recipient, or a recipient with no ingress, is nowhere.
func TestDeliveryAddress_IsZero(t *testing.T) {
	cases := []struct {
		name string
		addr domain.DeliveryAddress
		zero bool
	}{
		{"both", domain.DeliveryAddress{IngressAgentID: "tg", ExternalID: "1"}, false},
		{"no recipient", domain.DeliveryAddress{IngressAgentID: "tg"}, true},
		{"no ingress", domain.DeliveryAddress{ExternalID: "1"}, true},
		{"neither", domain.DeliveryAddress{}, true},
	}
	for _, c := range cases {
		if got := c.addr.IsZero(); got != c.zero {
			t.Errorf("%s: IsZero = %v, want %v", c.name, got, c.zero)
		}
	}

	// The audit rendering has to be unambiguous — an unbound conversation must not
	// print as something that looks like an address.
	if s := (domain.DeliveryAddress{}).String(); s != "<undeliverable>" {
		t.Errorf("unbound address renders as %q", s)
	}
	if s := (domain.DeliveryAddress{IngressAgentID: "tg", ExternalID: "12345"}).String(); s != "tg:12345" {
		t.Errorf("bound address renders as %q", s)
	}
}
