package chat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// The reply disclosure guard (T-20261108-records-request-replies-still-restate-private).
//
// A chat reply is the one piece of model output that leaves the kernel verbatim, and a
// prompt rule alone did not keep personal data out of it: four records-request replies in
// run 20261107T191751Z_usecases_full listed private persons' national ID and phone numbers
// as "to be redacted" — restating the very values they said must be withheld — even though
// chat_agent v3 and react-constraints v4 told the model not to. So every reply passes this
// deterministic check after the agent returns and before it is stored or delivered
// (TurnService.Turn), whatever agent the chat pool runs.
//
// What it masks — two rules, both generic and source-agnostic:
//
//  1. Withholding context, every conversation. A paragraph of the reply that says data is
//     redacted, withheld, blacked out or masked (withholdingCues, per catalogue language)
//     must not state a personal identifier: a reply cannot both declare a value withheld
//     and print it. Every identifier in such a paragraph is masked. A list set off by a
//     blank line under a withholding heading counts as part of it (withholdingParagraphs).
//  2. Customer-facing conversations (domain.ProfileCustomer, the requester is outside the
//     organisation). A strong identifier — a national ID number, an IBAN, a payment card
//     number — is masked anywhere in the reply. Phone numbers and e-mail addresses are left
//     here on purpose: an organisation's own contact details are the commonest reason a
//     reply to a customer carries one, and what a customer's turn can read is already
//     scoped by authorization.
//
// In both rules a value the user wrote in this conversation is theirs to see again and is
// never masked. Masked values are replaced by a label naming the category, and one line in
// the turn's language says what was held back, so the user knows something was left out
// and can look it up in the record if they are entitled to.
//
// What it does NOT do: recognise names. A private person's name (a record title "Letter
// from <name>") has no shape a deterministic check can tell from any other capitalised
// words; that stays with the drafting rules in the prompts (chat_agent v4). Nor does it
// look at text an agent writes into a work system through a tool — that is a separate
// path (follow-up ticket T-20261108-outbound-tool-writes-have-no-deterministic).

// identifierKind is one category of personal identifier the guard recognises.
type identifierKind string

const (
	idNationalID identifierKind = "national_id"
	idPhone      identifierKind = "phone"
	idEmail      identifierKind = "email"
	idIBAN       identifierKind = "iban"
	idCard       identifierKind = "card"
)

// identifierOrder is the fixed order the held-back line lists categories in.
var identifierOrder = []identifierKind{idNationalID, idPhone, idEmail, idIBAN, idCard}

// strongIdentifier reports whether a kind is masked anywhere in a customer-facing reply
// (rule 2), not only in a withholding paragraph.
func strongIdentifier(k identifierKind) bool {
	return k == idNationalID || k == idIBAN || k == idCard
}

// Identifier shapes. Each candidate is then checked by freeStanding and classified with the
// digit counts and checksums a regular expression cannot express.
var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// IBAN: country code, two check digits, then 11-30 alphanumerics, optionally in groups
	// of four. Validated with the ISO 13616 mod-97 check.
	reIBAN = regexp.MustCompile(`[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}`)
	// Letter-framed national numbers: the UK National Insurance number ("QQ 12 34 56 C").
	reNINO = regexp.MustCompile(`[A-Z]{2} ?\d{2} ?\d{2} ?\d{2} ?[A-D]`)
	// The US Social Security number.
	reSSN = regexp.MustCompile(`\d{3}-\d{2}-\d{4}`)
	// A payment card number in groups of four (Luhn-checked).
	reCard = regexp.MustCompile(`\d{4}(?:[ \-]?\d{4}){2,3}(?:[ \-]?\d{1,3})?`)
	// An unbroken run of digits (an eleven-digit national ID number, an ungrouped card).
	rePlainDigits = regexp.MustCompile(`\d+`)
	// A phone number: optional "+country", optional area code in parentheses (alone or
	// after a trunk prefix, "0 (509)"), then two to six groups of digits separated by one
	// space, dot or dash. classifyPhone then requires 10-15 digits in all.
	reNANP  = regexp.MustCompile(`^\d{3}[ .\-]\d{3}[ .\-]\d{4}$`)
	rePhone = regexp.MustCompile(`(?:\+\d{1,3}[ .\-]?)?(?:\(\d{1,5}\)[ .\-]?|\d{1,5}[ .\-]?\(\d{1,5}\)[ .\-]?)?\d{2,6}(?:[ .\-]\d{2,6}){1,5}`)
)

// detectedIdentifier is one identifier found in the reply, by byte offsets.
type detectedIdentifier struct {
	start, end int
	kind       identifierKind
	value      string
}

// findIdentifiers returns the identifiers in text, left to right, without overlaps.
func findIdentifiers(text string) []detectedIdentifier {
	var found []detectedIdentifier
	add := func(start, end int, kind identifierKind) {
		for _, f := range found {
			if start < f.end && f.start < end {
				return // an earlier, more specific shape already holds the span
			}
		}
		found = append(found, detectedIdentifier{start: start, end: end, kind: kind, value: text[start:end]})
	}
	each := func(re *regexp.Regexp, classify func(string) (identifierKind, bool)) {
		for _, m := range re.FindAllStringIndex(text, -1) {
			if !freeStanding(text, m[0], m[1]) {
				continue // part of a reference number, a date, an amount or a code
			}
			if kind, ok := classify(text[m[0]:m[1]]); ok {
				add(m[0], m[1], kind)
			}
		}
	}
	each(reEmail, func(string) (identifierKind, bool) { return idEmail, true })
	each(reIBAN, func(s string) (identifierKind, bool) { return idIBAN, validIBAN(s) })
	each(reNINO, func(string) (identifierKind, bool) { return idNationalID, true })
	each(reSSN, func(string) (identifierKind, bool) { return idNationalID, true })
	each(reCard, func(s string) (identifierKind, bool) { return idCard, luhnValid(onlyDigits(s)) })
	each(rePlainDigits, classifyPlain)
	each(rePhone, classifyPhone)
	sort.Slice(found, func(i, j int) bool { return found[i].start < found[j].start })
	return found
}

// freeStanding reports whether text[start:end] is a value of its own, not a piece of a
// longer token: a registry number ("E-74183520-310.04-1113"), a path ("HDC/CS/2025/00023"),
// a decimal or an amount ("1,977", "£935,300"). The character before must not be a letter,
// a digit or a joiner; the character after must not be a letter, a digit or a joiner
// either, except a full stop or comma that ends the sentence or clause.
func freeStanding(text string, start, end int) bool {
	if start > 0 {
		r := lastRune(text[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-/._,£$€₺#:+@", r) {
			return false
		}
	}
	if end < len(text) {
		r := firstRune(text[end:])
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-/_%@", r):
			return false
		case r == '.' || r == ',' || r == ':':
			// "...call 0555 123 45 67." ends a sentence; "12.5" or "1,977" continues a number.
			if rest := text[end+1:]; rest != "" && unicode.IsDigit(firstRune(rest)) {
				return false
			}
		}
	}
	return true
}

// classifyPlain classifies an unbroken digit run: eleven digits is the length of several
// national ID numbers (Turkey's T.C. kimlik no, Poland's PESEL, Brazil's CPF without its
// punctuation); 13-19 Luhn-valid digits is a card number. Anything else (a year, an
// amount, a short reference, a ten-digit ungrouped number) is left alone.
func classifyPlain(s string) (identifierKind, bool) {
	switch n := len(s); {
	case n == 11:
		return idNationalID, true
	case n >= 13 && n <= 19 && luhnValid(s):
		return idCard, true
	}
	return "", false
}

// classifyPhone accepts a grouped number of phone length (10-15 digits) written the way
// phone numbers are written: with an international "+", a trunk "0", an area code in
// parentheses, or the North American 3-3-4 grouping. That keeps a list of years ("2024 2025
// 2026") or of page numbers from reading as a phone number. Dates ("24.09.2026", 8 digits)
// are too short; a dot-grouped amount ("1.234.567.890", every group after the first three
// digits long) is an amount.
func classifyPhone(s string) (identifierKind, bool) {
	digits := len(onlyDigits(s))
	if digits < 10 || digits > 15 {
		return "", false
	}
	if !(strings.HasPrefix(s, "+") || strings.HasPrefix(s, "0") || strings.Contains(s, "(") ||
		reNANP.MatchString(s)) {
		return "", false
	}
	if !strings.ContainsAny(s, " -()+") {
		groups := strings.Split(s, ".")
		thousands := len(groups) > 1
		for _, g := range groups[1:] {
			if len(g) != 3 {
				thousands = false
			}
		}
		if thousands {
			return "", false
		}
	}
	return idPhone, true
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func luhnValid(d string) bool {
	if d == "" {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// validIBAN applies the ISO 13616 check: move the first four characters to the end, map
// letters to 10..35, and the number mod 97 must be 1.
func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			rem = (rem*10 + int(r-'0')) % 97
		case r >= 'A' && r <= 'Z':
			rem = (rem*100 + int(r-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

func lastRune(s string) rune {
	r := []rune(s)
	return r[len(r)-1]
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// withholdingCues are word stems that say data is being redacted or withheld, per catalogue
// language, written in fold() form (lower case, dotted and dotless i both as "i"). A
// paragraph holding any of them is a withholding paragraph (rule 1). The check reads every
// language's cues on every reply: a Turkish reply quoting an English heading is still
// declaring a redaction.
var withholdingCues = map[string][]string{
	"en": {"redact", "withh", "black out", "blacked out", "blank out", "blanked out",
		"mask", "anonymi", "not be disclosed", "not disclosed", "not be released",
		"held back", "obscure"},
	"tr": {"karart", "maskele", "gizlen", "gizli tutul", "anonimleştir", "anonimlestir",
		"verilmey", "verilmeyecek", "çikarilacak", "çikarilarak", "çikarildi", "silinecek",
		"silinerek", "örtül", "kapatilacak", "kapatilarak"},
}

// fold lower-cases for cue matching, mapping I, İ and ı all to "i" so English and Turkish
// casing rules give the same result ("WITHHELD", "KARARTILARAK", "VERİLMEYEN").
func fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'I', 'İ', 'ı':
			b.WriteRune('i')
		case '̇': // the combining dot some lower-casings leave after "i"
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func declaresWithholding(paragraph string) bool {
	p := fold(paragraph)
	for _, cues := range withholdingCues {
		for _, c := range cues {
			if strings.Contains(p, c) {
				return true
			}
		}
	}
	return false
}

// disclosureCatalogue holds the guard's fixed texts per catalogue language: the label put in
// place of each masked identifier and the held-back line. Same contract as
// messageCatalogue: every detectable language is complete
// (TestDisclosureCatalogue_EveryDetectableLanguageIsComplete).
var disclosureCatalogue = map[string]struct {
	label map[identifierKind]string // replaces the value
	noun  map[identifierKind]string // names the category in the held-back line
	line  string                    // %s = the categories held back
}{
	"en": {
		label: map[identifierKind]string{
			idNationalID: "[national ID number withheld]",
			idPhone:      "[phone number withheld]",
			idEmail:      "[e-mail address withheld]",
			idIBAN:       "[bank account number withheld]",
			idCard:       "[card number withheld]",
		},
		noun: map[identifierKind]string{
			idNationalID: "national ID number",
			idPhone:      "phone number",
			idEmail:      "e-mail address",
			idIBAN:       "bank account number",
			idCard:       "card number",
		},
		line: "Personal data left out of this reply: %s. It is in the source records for anyone entitled to see it.",
	},
	"tr": {
		label: map[identifierKind]string{
			idNationalID: "[kimlik numarası gizlendi]",
			idPhone:      "[telefon numarası gizlendi]",
			idEmail:      "[e-posta adresi gizlendi]",
			idIBAN:       "[banka hesap numarası gizlendi]",
			idCard:       "[kart numarası gizlendi]",
		},
		noun: map[identifierKind]string{
			idNationalID: "kimlik numarası",
			idPhone:      "telefon numarası",
			idEmail:      "e-posta adresi",
			idIBAN:       "banka hesap numarası",
			idCard:       "kart numarası",
		},
		line: "Bu yanıtta gösterilmeyen kişisel veriler: %s. Görmeye yetkili olanlar için kaynak kayıtlarda yer alıyor.",
	},
}

// heldBack counts what the guard masked, by category.
type heldBack map[identifierKind]int

func (h heldBack) total() int {
	n := 0
	for _, c := range h {
		n += c
	}
	return n
}

// guardDisclosure applies the two rules to reply. userText is everything the user wrote in
// this conversation (the latest message and the earlier user messages): a value found
// there is never masked. lang is the turn's language (the held-back line and the labels
// are said in it). It returns the reply to store and deliver, and what it held back.
func guardDisclosure(reply string, profile domain.ConversationProfile, userText []string, lang string) (string, heldBack) {
	ids := findIdentifiers(reply)
	if len(ids) == 0 {
		return reply, nil
	}
	supplied := map[string]bool{}
	for _, t := range userText {
		for _, id := range findIdentifiers(t) {
			supplied[normaliseIdentifier(id.value)] = true
		}
	}
	paragraphs := paragraphSpans(reply)
	withholding := withholdingParagraphs(reply, paragraphs)
	inWithholding := func(pos int) bool {
		for i, p := range paragraphs {
			if pos >= p[0] && pos < p[1] {
				return withholding[i]
			}
		}
		return false
	}

	cat, ok := disclosureCatalogue[normaliseLanguage(lang)]
	if !ok {
		cat = disclosureCatalogue[DefaultLanguage]
	}
	held := heldBack{}
	var b strings.Builder
	last := 0
	for _, id := range ids {
		if supplied[normaliseIdentifier(id.value)] {
			continue
		}
		mask := inWithholding(id.start) || (profile == domain.ProfileCustomer && strongIdentifier(id.kind))
		if !mask {
			continue
		}
		b.WriteString(reply[last:id.start])
		b.WriteString(cat.label[id.kind])
		last = id.end
		held[id.kind]++
	}
	if held.total() == 0 {
		return reply, nil
	}
	b.WriteString(reply[last:])
	var kinds []string
	for _, k := range identifierOrder {
		switch n := held[k]; {
		case n == 1:
			kinds = append(kinds, cat.noun[k])
		case n > 1:
			kinds = append(kinds, fmt.Sprintf("%s ×%d", cat.noun[k], n))
		}
	}
	return strings.TrimRight(b.String(), " \t\n") + "\n\n" + fmt.Sprintf(cat.line, strings.Join(kinds, ", ")), held
}

// userTexts is everything the user wrote in this conversation: the latest message and the
// earlier user messages in the loaded history.
func userTexts(latest string, history []domain.Message) []string {
	out := []string{latest}
	for _, m := range history {
		if m.Role == domain.MessageRoleUser {
			out = append(out, m.Content)
		}
	}
	return out
}

// normaliseIdentifier is the comparison form of an identifier: digits and letters only,
// upper-cased, so "0 (509) 735 17 39" and "05097351739" are the same number.
func normaliseIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) || unicode.IsLetter(r) || r == '@' || r == '.' {
			if r == '.' && !strings.Contains(s, "@") {
				continue
			}
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// withholdingParagraphs marks which paragraphs are withholding context (rule 1). A paragraph
// is one when it holds a withholding cue itself, or when it is a list that sits under a
// heading holding one: "Released with personal data redacted:" followed by a blank line and
// "- HDC/CS/2025/00023 – …" items puts the cue and the items in different paragraphs, and
// the items are exactly what the heading says is being withheld
// (T-20261108-reply-disclosure-guard-does-not-mask-a-private). The heading's scope runs over
// the list paragraphs right after it and ends at the first paragraph that is not a list
// (prose or another heading), so a later section of the reply is never swept in.
func withholdingParagraphs(text string, paragraphs [][2]int) []bool {
	out := make([]bool, len(paragraphs))
	underWithholdingHeading := false
	for i, p := range paragraphs {
		para := text[p[0]:p[1]]
		cue := declaresWithholding(para)
		switch {
		case cue:
			out[i] = true
			underWithholdingHeading = isHeading(para) || endsWithHeading(para)
		case underWithholdingHeading && isListBlock(para):
			out[i] = true
		default:
			underWithholdingHeading = false
		}
	}
	return out
}

// isHeading reports whether a paragraph is a single heading line: a markdown heading
// ("## Redacted records") or a line that introduces what follows ("Released with
// redactions:", "**Withheld in part:**").
func isHeading(paragraph string) bool {
	lines := nonBlankLines(paragraph)
	return len(lines) == 1 && headingLine(lines[0])
}

// endsWithHeading reports whether a paragraph's last line is a heading: prose that ends by
// introducing a list ("I have prepared the response.\nReleased with redactions:").
func endsWithHeading(paragraph string) bool {
	lines := nonBlankLines(paragraph)
	return len(lines) > 0 && headingLine(lines[len(lines)-1]) && !listLine(lines[len(lines)-1])
}

func headingLine(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return true
	}
	t = strings.TrimRight(t, "*_ ")
	return strings.HasSuffix(t, ":")
}

// isListBlock reports whether every line of a paragraph is a list item or an indented
// continuation of one.
func isListBlock(paragraph string) bool {
	lines := nonBlankLines(paragraph)
	if len(lines) == 0 {
		return false
	}
	for i, ln := range lines {
		if listLine(ln) {
			continue
		}
		if i > 0 && (strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t")) {
			continue // an indented continuation of the item above
		}
		return false
	}
	return true
}

// listLine reports whether a line is a bulleted ("-", "*", "+", "•") or numbered ("1.",
// "2)") list item.
func listLine(line string) bool {
	t := strings.TrimSpace(line)
	for _, b := range []string{"- ", "* ", "+ ", "• ", "– "} {
		if strings.HasPrefix(t, b) {
			return true
		}
	}
	n := 0
	for n < len(t) && t[n] >= '0' && t[n] <= '9' {
		n++
	}
	return n > 0 && n < len(t) && (t[n] == '.' || t[n] == ')')
}

func nonBlankLines(paragraph string) []string {
	var out []string
	for _, ln := range strings.Split(paragraph, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, strings.TrimRight(ln, "\r"))
		}
	}
	return out
}

// paragraphSpans splits text into blank-line separated paragraphs, as byte spans.
func paragraphSpans(text string) [][2]int {
	var spans [][2]int
	start := 0
	lines := strings.SplitAfter(text, "\n")
	pos := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			if pos > start {
				spans = append(spans, [2]int{start, pos})
			}
			start = pos + len(ln)
		}
		pos += len(ln)
	}
	if pos > start {
		spans = append(spans, [2]int{start, pos})
	}
	return spans
}
