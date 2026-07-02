package domain

import (
	"regexp"
	"strings"
)

// ADR-0051 D13 — the findings injection boundary. Scout's STRUCTURED observations (entity
// kind/id/exists/content_cid) are deterministic world-state and trusted. Its GENERATIVE text
// (the thin interpretation + entity summaries) and any raw text are UNTRUSTED — a poisoned
// MCP read could carry an instruction aimed at the Planner, the highest-stakes consumer.
// sanitizeUntrusted defangs that text before it enters the Planner prompt; the structured
// facts bypass it. No reusable MCP scanner existed, so this is the boundary's implementation.

const maxUntrustedLen = 400 // a thin interpretation/summary is one sentence; longer = suspect

// injectionPhrases are case-insensitive prompt-injection triggers redacted from untrusted
// text. Not exhaustive — the primary defense is the structural escape (a poisoned line can
// carry words but never break out of its tag or issue a real directive the Planner trusts).
var injectionPhrases = regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b[^.\n]*\b(previous|prior|above|preceding|earlier|all)\b[^.\n]*\binstructions?\b` +
	`|(?i)\b(system|developer)\s*(prompt|message|:)` +
	`|(?i)\byou\s+are\s+now\b` +
	`|(?i)\bnew\s+instructions?\s*:` +
	`|(?i)\b(grant|give)\b[^.\n]*\b(admin|root|all\s+access|permission)`)

// escapeXMLContent escapes the characters that let untrusted text break out of its prompt
// element (the structural-breakout vector — a literal </DiscoveryLTM> or <System> in the
// text). This is the load-bearing defense.
func escapeXMLContent(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;") // safe in element content; required in attribute values
	return s
}

// sanitizeUntrusted neutralizes LLM-generated free text destined for the Planner prompt
// (ADR-0051 D13): (1) escape angle brackets so it cannot open/close prompt tags, (2) redact
// known injection trigger phrases, (3) cap length. Deterministic.
func sanitizeUntrusted(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxUntrustedLen {
		s = s[:maxUntrustedLen] + "…"
	}
	s = injectionPhrases.ReplaceAllString(s, "[filtered]")
	return escapeXMLContent(s)
}
