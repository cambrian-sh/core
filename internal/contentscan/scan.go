// Package contentscan is the kernel's deterministic scanner for text that will be
// shown to a model as INSTRUCTIONS: agent-proposed skills (ADR-0139 D13) and
// principal memory notes (ADR-0139 D14, app/memory_notes.go). It is shared on
// purpose (AGENTS.md §4.3): one rule set, one verdict vocabulary, so the two
// write paths cannot drift into two different ideas of "dangerous".
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
	"sort"
	"strings"
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

// rules is the fixed rule set. Patterns are case-insensitive and deliberately
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

// invisible flags zero-width and bidi-control characters: text an operator reviewing
// the proposal cannot see, but a model reads. Dangerous — there is no legitimate
// reason for a skill to carry them.
var invisible = regexp.MustCompile(`[\x{200b}\x{200c}\x{200d}\x{2060}\x{feff}\x{202a}-\x{202e}\x{2066}-\x{2069}]`)

// longBase64 flags a long opaque blob, a common carrier for an encoded payload.
var longBase64 = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)

// quoted reports whether the match at loc sits inside a quoted span on its own
// line: an odd number of double quotes (straight or a curly opener) between the
// line start and the match, and a closing quote after it on the same line.
func quoted(text string, loc []int) bool {
	start := strings.LastIndexByte(text[:loc[0]], '\n') + 1
	end := len(text)
	if i := strings.IndexByte(text[loc[1]:], '\n'); i >= 0 {
		end = loc[1] + i
	}
	before, after := text[start:loc[0]], text[loc[1]:end]
	opens := strings.Count(before, `"`) + strings.Count(before, "“") - strings.Count(before, "”")
	return opens%2 == 1 && (strings.Contains(after, `"`) || strings.Contains(after, "”"))
}

// Scan runs every rule over text and returns the verdict and findings. Pure and
// deterministic: the same text always yields the same Result.
func Scan(text string) Result {
	var findings []Finding
	lineOf := func(idx int) int { return strings.Count(text[:idx], "\n") + 1 }
	add := func(id string, class Class, sev severity, loc []int) {
		ex := strings.TrimSpace(text[loc[0]:loc[1]])
		if len([]rune(ex)) > 80 {
			ex = string([]rune(ex)[:80])
		}
		s := "caution"
		if sev == sevDanger {
			s = "danger"
		}
		findings = append(findings, Finding{Rule: id, Class: class, Severity: s, Line: lineOf(loc[0]), Excerpt: ex})
	}
	for _, r := range rules {
		if loc := r.re.FindStringIndex(text); loc != nil {
			sev := r.sev
			// A phrase QUOTED in the text is a mention, not an instruction: the
			// shipped web-research skill warns agents about pages that say
			// "ignore your previous instructions". It is still reported (an
			// attacker can quote too), but as caution, so an operator decides.
			if sev == sevDanger && quoted(text, loc) {
				sev = sevCaution
			}
			add(r.id, r.class, sev, loc)
		}
	}
	if loc := invisible.FindStringIndex(text); loc != nil {
		add("OB-01", ClassObfuscation, sevDanger, loc)
		// The excerpt of an invisible character is invisible; say what it was.
		findings[len(findings)-1].Excerpt = fmt.Sprintf("U+%04X", []rune(text[loc[0]:loc[1]])[0])
	}
	if loc := longBase64.FindStringIndex(text); loc != nil {
		add("OB-02", ClassObfuscation, sevCaution, loc)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Rule < findings[j].Rule
	})

	v := VerdictClean
	for _, f := range findings {
		if f.Severity == "danger" {
			v = VerdictDangerous
			break
		}
		v = VerdictSuspicious
	}
	return Result{Verdict: v, Findings: findings}
}
