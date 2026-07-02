package domain

import (
	"strings"
	"testing"
)

// Cycle 4 — RegexPIIMasker redacts email addresses.
func TestRegexPIIMasker_MasksEmail(t *testing.T) {
	m := NewRegexPIIMasker()
	got := m.Mask("contact alice@example.com for details")
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("email should be redacted, got: %q", got)
	}
	if !strings.Contains(got, "contact") || !strings.Contains(got, "for details") {
		t.Errorf("surrounding text should be preserved, got: %q", got)
	}
}

// Cycle 5 — RegexPIIMasker redacts international phone numbers.
func TestRegexPIIMasker_MasksPhone(t *testing.T) {
	m := NewRegexPIIMasker()
	cases := []string{
		"+1 555-123-4567",
		"(555) 123-4567",
		"555 123 4567",
	}
	for _, phone := range cases {
		got := m.Mask("call us at " + phone + " today")
		if strings.Contains(got, phone) {
			t.Errorf("phone %q should be redacted, got: %q", phone, got)
		}
	}
}

// Cycle 6 — RegexPIIMasker redacts customer/order/tenant/user numeric ID patterns.
func TestRegexPIIMasker_MasksNumericIDs(t *testing.T) {
	m := NewRegexPIIMasker()
	cases := []struct {
		input   string
		pattern string
	}{
		{"assigned to customer_1234", "customer_1234"},
		{"see order_9876 for reference", "order_9876"},
		{"tenant_0042 has access", "tenant_0042"},
		{"user_5555 approved this", "user_5555"},
	}
	for _, tc := range cases {
		got := m.Mask(tc.input)
		if strings.Contains(got, tc.pattern) {
			t.Errorf("pattern %q should be redacted in %q, got: %q", tc.pattern, tc.input, got)
		}
	}
}

// Cycle 7 — RegexPIIMasker passes clean decision text through unchanged.
func TestRegexPIIMasker_PassesThroughCleanText(t *testing.T) {
	m := NewRegexPIIMasker()
	clean := "we decided to use JWT for authentication and refresh tokens every 24 hours"
	got := m.Mask(clean)
	if got != clean {
		t.Errorf("clean text should pass through unchanged\nwant: %q\ngot:  %q", clean, got)
	}
}

// Cycle 8 — RegexPIIMasker redacts PII while preserving surrounding decision text.
func TestRegexPIIMasker_PreservesDecisionTextAroundPII(t *testing.T) {
	m := NewRegexPIIMasker()
	input := "we agreed that bob@company.com will own the deployment pipeline"
	got := m.Mask(input)
	if strings.Contains(got, "bob@company.com") {
		t.Errorf("email should be redacted, got: %q", got)
	}
	if !strings.Contains(got, "we agreed that") {
		t.Errorf("decision context should be preserved, got: %q", got)
	}
	if !strings.Contains(got, "will own the deployment pipeline") {
		t.Errorf("decision tail should be preserved, got: %q", got)
	}
}

// Cycle 9 — Empty string passes through without panic.
func TestRegexPIIMasker_EmptyString(t *testing.T) {
	m := NewRegexPIIMasker()
	got := m.Mask("")
	if got != "" {
		t.Errorf("empty input: want %q got %q", "", got)
	}
}
