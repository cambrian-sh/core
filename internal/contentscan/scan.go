// Package contentscan is the kernel's ONE deterministic scanner for injection
// patterns in text a model will read: agent-proposed skills (ADR-0139 D13) and
// principal memory notes (ADR-0139 D14, app/memory_notes.go), shown to a model as
// INSTRUCTIONS, and MCP tool responses (ADR-0043 D4, ENG-117), shown as untrusted
// DATA. It is shared on purpose (AGENTS.md §4.3): one rule set, one verdict
// vocabulary, so the write paths and the tool-output path cannot drift into
// different ideas of "dangerous". Ingest (ENG-94) is meant to call it too.
//
// Text is normalised before the rules run (normalize: NFKC, invisible characters
// removed, common Cyrillic/Greek homoglyphs folded to Latin), so full-width
// letters, a zero-width space inside a word and a Cyrillic o in "ignore" all
// read as "ignore". The obfuscation rules (OB-*) look at the text as written,
// because the obfuscation itself is the signal.
//
// The package default (Scan) is the built-in rule set. A deployment may narrow or
// extend it per consumer (New with Config: disabled_rules, extra_rules); the MCP
// output scan reads that from execution.tools.output_scan.
//
// It is a pattern scanner, not a judge. There is no LLM here, by design (ADR-0139
// D13: "a dangerous verdict cannot be overridden" is only a meaningful promise if
// the verdict is reproducible). It is tuned to flag the four families the
// ClawHub-style skill campaigns actually used:
//
//   - prompt-injection markers (instruction-override phrasing, chat-template tokens,
//     invisible/bidi characters);
//   - credential requests (asking the user or operator to hand over a secret);
//   - exfiltration instructions (sending secrets or files to a remote endpoint);
//   - tool-policy override text (bypass approval, disable the sandbox, ...).
//
// A false positive costs an operator one look at a "suspicious" proposal; a false
// negative is mitigated by the approval queue itself (nothing agent-proposed is
// ever active unapproved). The scanner is therefore a floor, not the gate.
package contentscan

import (
	"fmt"
	"regexp"
)

// Verdict is the scan outcome. The strings are the operator-plane vocabulary
// (operator.proto SkillProposalOp.scan_verdict).
type Verdict string

const (
	// VerdictClean: no rule matched.
	VerdictClean Verdict = "clean"
	// VerdictSuspicious: only caution-level rules matched; an operator may approve.
	VerdictSuspicious Verdict = "suspicious"
	// VerdictDangerous: at least one danger-level rule matched. A dangerous verdict
	// can never be approved (ADR-0139 D13).
	VerdictDangerous Verdict = "dangerous"
)

// Class is the rule family a finding belongs to.
type Class string

const (
	ClassPromptInjection   Class = "prompt_injection"
	ClassCredentialRequest Class = "credential_request"
	ClassExfiltration      Class = "exfiltration"
	ClassToolPolicy        Class = "tool_policy_override"
	ClassObfuscation       Class = "obfuscation"
)

type severity int

const (
	sevCaution severity = iota + 1
	sevDanger
)

// Finding is one rule match.
type Finding struct {
	Rule     string
	Class    Class
	Severity string // "caution" | "danger"
	Line     int    // 1-based line of the first match
	Excerpt  string // the matched text, trimmed to 80 chars
}

// String renders the finding as the one-line form the operator plane shows.
func (f Finding) String() string {
	return fmt.Sprintf("%s [%s/%s] line %d: %q", f.Rule, f.Class, f.Severity, f.Line, f.Excerpt)
}

// Result is a scan's verdict plus every finding, in line order.
type Result struct {
	Verdict  Verdict
	Findings []Finding
}

// FindingStrings renders the findings for a wire field (repeated string).
func (r Result) FindingStrings() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.String())
	}
	return out
}

type rule struct {
	id    string
	class Class
	sev   severity
	re    *regexp.Regexp
}

// rules is the built-in phrase rule set (matched against normalised text). Patterns are case-insensitive and deliberately
// phrase-shaped (verb + object) so that a skill which merely TALKS about secrets
// ("never print an API key") does not trip the exfiltration rules.
var rules = []rule{
	// ── prompt injection ────────────────────────────────────────────────────
	{"PI-01", ClassPromptInjection, sevDanger, regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.\n]{0,30}\b(all\s+)?(previous|prior|above|earlier|preceding|system|your)\s+(instructions?|rules|prompts?|guidelines|directives)`)},
	{"PI-02", ClassPromptInjection, sevDanger, regexp.MustCompile(`(?i)\byou\s+are\s+now\s+(in\s+)?(developer|dan|jailbreak|unrestricted|god)\s*mode\b`)},
	{"PI-03", ClassPromptInjection, sevDanger, regexp.MustCompile(`(?i)(<\|im_start\|>|<\|im_end\|>|<\|system\|>|<\|endoftext\|>|\[/?INST\]|<<\s*/?SYS\s*>>|</?\s*system\s*>)`)},
	{"PI-04", ClassPromptInjection, sevDanger, regexp.MustCompile(`(?i)\b(new|updated|real|actual)\s+system\s+prompt\s*:`)},
	{"PI-05", ClassPromptInjection, sevCaution, regexp.MustCompile(`(?i)\b(do\s+not|don't|never)\s+(tell|inform|reveal\s+to|mention\s+(this\s+)?to)\s+(the\s+)?(user|operator|anyone|human)`)},
	{"PI-06", ClassPromptInjection, sevCaution, regexp.MustCompile(`(?i)\b(reveal|print|output|repeat|dump)\s+(your|the)\s+(system\s+prompt|hidden\s+instructions|initial\s+instructions)`)},

	// ── credential requests ────────────────────────────────────────────────
	{"CR-01", ClassCredentialRequest, sevDanger, regexp.MustCompile(`(?i)\b(ask|request|prompt|require)\s+(the\s+)?(user|operator|them|human|customer)\s+(for|to\s+(provide|enter|share|paste|send|give))\s+(their|your|the|his|her)?\s*(password|passphrase|api[\s_-]?keys?|credentials?|access\s+tokens?|secret(\s+key)?s?|private\s+keys?|seed\s+phrase|2fa|otp|mfa\s+codes?)`)},
	{"CR-02", ClassCredentialRequest, sevDanger, regexp.MustCompile(`(?i)\b(enter|provide|share|paste|send|give\s+me)\s+your\s+(password|passphrase|api[\s_-]?keys?|credentials?|access\s+tokens?|secret\s+keys?|private\s+keys?|seed\s+phrase|recovery\s+codes?)`)},

	// ── exfiltration ────────────────────────────────────────────────────────
	{"EX-01", ClassExfiltration, sevDanger, regexp.MustCompile(`(?i)\b(send|post|upload|exfiltrate|forward|transmit|email|leak|copy)\b[^.\n]{0,60}\b(credentials?|secrets?|api[\s_-]?keys?|access\s+tokens?|passwords?|env(ironment)?\s+var(iable)?s?|\.env\b|ssh\s+keys?|id_rsa|private\s+keys?|cookies)\b[^.\n]{0,60}\b(to|at|into)\s+(https?://|[a-z0-9-]+\.[a-z]{2,}|an?\s+(external|remote)|my|our|the\s+(attacker|following))`)},
	{"EX-02", ClassExfiltration, sevDanger, regexp.MustCompile(`(?i)\b(curl|wget|invoke-webrequest|iwr|nc|netcat)\b[^\n]{0,120}(\$\{?[A-Z_]*(KEY|TOKEN|SECRET|PASSWORD)[A-Z_]*\}?|\.env\b|id_rsa|\.ssh/|/etc/shadow|credentials\.json)`)},
	{"EX-03", ClassExfiltration, sevDanger, regexp.MustCompile(`(?i)\b(webhook\.site|requestbin\.(com|net)|pipedream\.net|interact\.sh|burpcollaborator\.net|oast\.(fun|me|pro)|ngrok(-free)?\.(io|app))\b`)},
	{"EX-04", ClassExfiltration, sevCaution, regexp.MustCompile(`(?i)(~/\.ssh\b|/etc/passwd\b|/etc/shadow\b|\.aws/credentials\b|\.netrc\b)`)},

	// ── tool-policy override ────────────────────────────────────────────────
	{"TP-01", ClassToolPolicy, sevDanger, regexp.MustCompile(`(?i)\b(bypass|disable|skip|ignore|circumvent|turn\s+off|evade|work\s+around)\b[^.\n]{0,20}\b(the\s+)?(operator\s+)?(approvals?|tool\s+polic(y|ies)|permissions?|sandbox(ing)?|guardrails?|safety\s+(checks?|filters?)|hooks?|scope\s+checks?|authori[sz]ation)\b`)},
	{"TP-02", ClassToolPolicy, sevDanger, regexp.MustCompile(`(?i)\b(without|no\s+need\s+(for|to\s+ask\s+for))\s+(asking\s+(for\s+)?)?(the\s+)?(operator'?s?\s+|user'?s?\s+)?(approval|permission|confirmation)\b`)},
	{"TP-03", ClassToolPolicy, sevDanger, regexp.MustCompile(`(?i)\b(tools_unrestricted|dangerously[-_]skip[-_]permissions|--no-sandbox|allow_all_tools)\b`)},
	{"TP-04", ClassToolPolicy, sevCaution, regexp.MustCompile(`(?i)\b(rm\s+-rf\s+(/|~|\$HOME)|mkfs\.|dd\s+if=.{0,40}of=/dev/)`)},
}

// Scan runs the built-in rule set over text. Pure and deterministic: the same
// text always yields the same Result.
func Scan(text string) Result { return defaultScanner.Scan(text) }
