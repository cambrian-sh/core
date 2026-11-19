package contentscan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The obfuscation rules look at the text as written (after NFKC only), not the
// normalised text, because what they flag is the hiding itself.
const (
	ruleInvisible  = "OB-01" // zero-width / bidi-control characters
	ruleBase64     = "OB-02" // a long opaque blob
	ruleTagChars   = "OB-03" // Unicode tag characters (U+E0000–U+E007F)
	ruleMixedWords = "OB-04" // a word mixing Latin with Cyrillic or Greek letters
)

// builtinIDs is every built-in rule id, for validating disabled_rules and
// refusing an extra rule that would shadow one.
func builtinIDs() map[string]bool {
	ids := map[string]bool{ruleInvisible: true, ruleBase64: true, ruleTagChars: true, ruleMixedWords: true}
	for _, r := range rules {
		ids[r.id] = true
	}
	return ids
}

// invisible flags zero-width and bidi-control characters: text a person reviewing
// it cannot see, but a model reads. Dangerous — there is no legitimate reason for
// an instruction text to carry them. Three uses are legitimate and NOT flagged
// (flaggedInvisible): a zero-width joiner inside an emoji or Indic sequence, a
// zero-width non-joiner between non-Latin letters (Persian), and a byte-order
// mark at the very start of the text. Tool output (web pages, chat messages)
// carries emoji sequences all the time.
var invisible = regexp.MustCompile(`[\x{200b}\x{200c}\x{200d}\x{2060}\x{feff}\x{202a}-\x{202e}\x{2066}-\x{2069}]`)

// tagChars flags Unicode tag characters: invisible, and a known carrier for
// "ASCII smuggling" (each tag character mirrors an ASCII one a model can read).
var tagChars = regexp.MustCompile(`[\x{e0000}-\x{e007f}]`)

// mixedWord flags a Latin letter directly beside a Cyrillic or Greek one — the
// homoglyph trick ("ignоre" with a Cyrillic о). Caution only: mathematical text
// ("αx") does this legitimately.
var mixedWord = regexp.MustCompile(`\p{Latin}[\p{Cyrillic}\p{Greek}]|[\p{Cyrillic}\p{Greek}]\p{Latin}`)

// longBase64 flags a long opaque blob, a common carrier for an encoded payload.
var longBase64 = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)

// stripped are the characters normalize removes before the phrase rules run: the
// invisible set above plus soft hyphen, the directional marks, the invisible math
// operators, the Mongolian vowel separator and the tag characters. Removing them
// rejoins a phrase an attacker split ("ig<U+200B>nore").
func stripped(r rune) bool {
	switch {
	case r == 0x00ad, r == 0x180e, r == 0xfeff:
		return true
	case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069,
		r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

// confusables folds the Cyrillic and Greek letters that render like Latin ones
// (the Unicode confusables most used in homoglyph attacks). The phrase rules are
// case-insensitive, so upper and lower case both map.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'к': 'k', 'м': 'm',
	'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y',
	'х': 'x', 'ѕ': 's', 'і': 'i', 'ї': 'i', 'ј': 'j', 'ԁ': 'd',
	'ԛ': 'q', 'ԝ': 'w', 'һ': 'h', 'ӏ': 'l',
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H',
	'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X', 'У': 'Y',
	'Ѕ': 'S', 'І': 'I', 'Ј': 'J', 'Ԁ': 'D', 'Ԛ': 'Q', 'Ԝ': 'W',
	'Һ': 'H', 'Ӏ': 'I',
	// Greek
	'α': 'a', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o',
	'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I',
	'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T',
	'Υ': 'Y', 'Χ': 'X',
	// Latin small script g
	'ɡ': 'g',
}

// normalize is the text the phrase rules read: NFKC (full-width and mathematical
// letters become ASCII, ligatures split), the stripped characters removed,
// confusables folded. Line breaks are kept, so a finding's line still points at
// the original line.
func normalize(text string) string {
	text = norm.NFKC.String(text)
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if stripped(r) {
			continue
		}
		if m, ok := confusables[r]; ok {
			r = m
		}
		b.WriteRune(r)
	}
	return b.String()
}

// flaggedInvisible returns the location of the first invisible character that is
// not one of the legitimate uses (see invisible), or nil.
func flaggedInvisible(text string) []int {
	for _, loc := range invisible.FindAllStringIndex(text, -1) {
		r, _ := utf8.DecodeRuneInString(text[loc[0]:])
		before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
		after, _ := utf8.DecodeRuneInString(text[loc[1]:])
		latinNeighbour := unicode.Is(unicode.Latin, before) || unicode.Is(unicode.Latin, after)
		switch {
		case r == 0xfeff && loc[0] == 0:
			continue // a byte-order mark
		case (r == 0x200d || r == 0x200c) && !latinNeighbour:
			continue // an emoji/Indic joiner, a Persian non-joiner
		}
		return loc
	}
	return nil
}

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

// RuleSpec is one deployment-defined rule (an extra_rules entry): an id that does
// not shadow a built-in one, a Class, a severity ("caution" | "danger") and an
// RE2 pattern matched against the normalised text (add (?i) for case-insensitive).
type RuleSpec struct {
	ID       string
	Class    string
	Severity string
	Pattern  string
}

// Config narrows or extends the built-in rule set for one consumer.
type Config struct {
	// DisabledRules are built-in rule ids to skip (each must exist).
	DisabledRules []string
	// ExtraRules are added after the built-in ones.
	ExtraRules []RuleSpec
}

// Scanner is a rule set built from a Config. Build one with New; it is
// immutable and safe for concurrent use.
type Scanner struct {
	rules    []rule
	disabled map[string]bool
}

var defaultScanner = &Scanner{rules: rules, disabled: map[string]bool{}}

// New builds a Scanner: the built-in rules minus cfg.DisabledRules, plus
// cfg.ExtraRules. An unknown disabled id, a duplicate or shadowing extra id, an
// unknown class or severity, or a pattern that does not compile is an error — a
// misconfigured scanner refuses to build rather than silently scanning less.
func New(cfg Config) (*Scanner, error) {
	builtin := builtinIDs()
	s := &Scanner{disabled: map[string]bool{}}
	for _, id := range cfg.DisabledRules {
		id = strings.TrimSpace(id)
		if !builtin[id] {
			return nil, fmt.Errorf("contentscan: disabled rule %q is not a built-in rule", id)
		}
		s.disabled[id] = true
	}
	for _, r := range rules {
		if !s.disabled[r.id] {
			s.rules = append(s.rules, r)
		}
	}
	seen := map[string]bool{}
	for _, x := range cfg.ExtraRules {
		id := strings.TrimSpace(x.ID)
		switch {
		case id == "":
			return nil, fmt.Errorf("contentscan: an extra rule has no id")
		case builtin[id] || seen[id]:
			return nil, fmt.Errorf("contentscan: extra rule id %q is already used", id)
		}
		seen[id] = true
		class := Class(strings.TrimSpace(x.Class))
		if !knownClass(class) {
			return nil, fmt.Errorf("contentscan: extra rule %s: unknown class %q", id, x.Class)
		}
		var sev severity
		switch strings.TrimSpace(x.Severity) {
		case "caution":
			sev = sevCaution
		case "danger":
			sev = sevDanger
		default:
			return nil, fmt.Errorf("contentscan: extra rule %s: severity must be caution or danger, got %q", id, x.Severity)
		}
		if x.Pattern == "" {
			return nil, fmt.Errorf("contentscan: extra rule %s: empty pattern", id)
		}
		re, err := regexp.Compile(x.Pattern)
		if err != nil {
			return nil, fmt.Errorf("contentscan: extra rule %s: pattern: %w", id, err)
		}
		s.rules = append(s.rules, rule{id: id, class: class, sev: sev, re: re})
	}
	return s, nil
}

// Classes are the rule families an extra rule may belong to.
func Classes() []Class {
	return []Class{ClassPromptInjection, ClassCredentialRequest, ClassExfiltration, ClassToolPolicy, ClassObfuscation}
}

func knownClass(c Class) bool {
	for _, k := range Classes() {
		if c == k {
			return true
		}
	}
	return false
}

// Scan runs the scanner's rules over text and returns the verdict and findings,
// in line order. Pure and deterministic.
func (s *Scanner) Scan(text string) Result {
	var findings []Finding
	add := func(src, id string, class Class, sev severity, loc []int) {
		ex := strings.TrimSpace(src[loc[0]:loc[1]])
		if len([]rune(ex)) > 80 {
			ex = string([]rune(ex)[:80])
		}
		sv := "caution"
		if sev == sevDanger {
			sv = "danger"
		}
		line := strings.Count(src[:loc[0]], "\n") + 1
		findings = append(findings, Finding{Rule: id, Class: class, Severity: sv, Line: line, Excerpt: ex})
	}
	normal := normalize(text)
	for _, r := range s.rules {
		if loc := r.re.FindStringIndex(normal); loc != nil {
			sev := r.sev
			// A phrase QUOTED in the text is a mention, not an instruction: the
			// shipped web-research skill warns agents about pages that say
			// "ignore your previous instructions". It is still reported (an
			// attacker can quote too), but as caution, so an operator decides.
			if sev == sevDanger && quoted(normal, loc) {
				sev = sevCaution
			}
			add(normal, r.id, r.class, sev, loc)
		}
	}
	nfkc := norm.NFKC.String(text)
	// The excerpt of an invisible character is invisible; say what it was.
	describe := func(loc []int) {
		r, _ := utf8.DecodeRuneInString(nfkc[loc[0]:])
		findings[len(findings)-1].Excerpt = fmt.Sprintf("U+%04X", r)
	}
	if !s.disabled[ruleInvisible] {
		if loc := flaggedInvisible(nfkc); loc != nil {
			add(nfkc, ruleInvisible, ClassObfuscation, sevDanger, loc)
			describe(loc)
		}
	}
	if !s.disabled[ruleTagChars] {
		if loc := tagChars.FindStringIndex(nfkc); loc != nil {
			add(nfkc, ruleTagChars, ClassObfuscation, sevDanger, loc)
			describe(loc)
		}
	}
	if !s.disabled[ruleMixedWords] {
		if loc := mixedWord.FindStringIndex(nfkc); loc != nil {
			add(nfkc, ruleMixedWords, ClassObfuscation, sevCaution, loc)
		}
	}
	if !s.disabled[ruleBase64] {
		if loc := longBase64.FindStringIndex(nfkc); loc != nil {
			add(nfkc, ruleBase64, ClassObfuscation, sevCaution, loc)
		}
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
