package domain

import "regexp"

// PIIMasker redacts personally identifiable information from text before LTM storage.
// Applied at the episodic extraction boundary — after SessionEvent retrieval,
// before any LLM call or pgvector write. ADR-0029, REQ-CHATBOT-001.
type PIIMasker interface {
	Mask(text string) string
}

// RegexPIIMasker implements PIIMasker using compiled regular expressions.
// Covers emails, phone numbers, and numeric ID patterns for customer/order/tenant/user.
type RegexPIIMasker struct {
	email     *regexp.Regexp
	phone     *regexp.Regexp
	numericID *regexp.Regexp
}

// NewRegexPIIMasker constructs a RegexPIIMasker with pre-compiled patterns.
func NewRegexPIIMasker() *RegexPIIMasker {
	return &RegexPIIMasker{
		email:     regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
		phone:     regexp.MustCompile(`(\+?[0-9]{1,3}[\s\-]?)?(\([0-9]{2,4}\)[\s\-]?)?[0-9]{3,4}[\s\-][0-9]{4}`),
		numericID: regexp.MustCompile(`(?i)(customer|order|tenant|user)[_\-]?[0-9]{4,}`),
	}
}

// Mask redacts PII patterns from text, returning the sanitised string.
func (m *RegexPIIMasker) Mask(text string) string {
	out := m.email.ReplaceAllString(text, "[REDACTED_EMAIL]")
	out = m.phone.ReplaceAllString(out, "[REDACTED_PHONE]")
	out = m.numericID.ReplaceAllString(out, "[REDACTED_ID]")
	return out
}
