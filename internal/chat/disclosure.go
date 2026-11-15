package chat

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identifiers"
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
// look at text an agent writes into a work system through a tool — that is the executor's
// outbound identifier guard (domain/outbound_identifiers.go), which runs the same detector
// over the arguments of every egress_send call and sends a hit to approval.

// The detector is the one shared implementation (internal/identifiers), also run by the
// executor's outbound identifier guard over tool arguments
// (T-20261108-outbound-tool-writes-have-no-deterministic) — never a second copy here.
// What this file owns is the reply POLICY: which kinds are masked where, the labels and
// the held-back line.

// identifierOrder is the fixed order the held-back line lists categories in.
var identifierOrder = domain.AllIdentifierKinds

// strongIdentifier reports whether a kind is masked anywhere in a customer-facing reply
// (rule 2), not only in a withholding paragraph.
func strongIdentifier(k domain.IdentifierKind) bool {
	return k == domain.IdentifierNationalID || k == domain.IdentifierIBAN || k == domain.IdentifierCard
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
	label map[domain.IdentifierKind]string // replaces the value
	noun  map[domain.IdentifierKind]string // names the category in the held-back line
	line  string                           // %s = the categories held back
}{
	"en": {
		label: map[domain.IdentifierKind]string{
			domain.IdentifierNationalID: "[national ID number withheld]",
			domain.IdentifierPhone:      "[phone number withheld]",
			domain.IdentifierEmail:      "[e-mail address withheld]",
			domain.IdentifierIBAN:       "[bank account number withheld]",
			domain.IdentifierCard:       "[card number withheld]",
		},
		noun: map[domain.IdentifierKind]string{
			domain.IdentifierNationalID: "national ID number",
			domain.IdentifierPhone:      "phone number",
			domain.IdentifierEmail:      "e-mail address",
			domain.IdentifierIBAN:       "bank account number",
			domain.IdentifierCard:       "card number",
		},
		line: "Personal data left out of this reply: %s. It is in the source records for anyone entitled to see it.",
	},
	"tr": {
		label: map[domain.IdentifierKind]string{
			domain.IdentifierNationalID: "[kimlik numarası gizlendi]",
			domain.IdentifierPhone:      "[telefon numarası gizlendi]",
			domain.IdentifierEmail:      "[e-posta adresi gizlendi]",
			domain.IdentifierIBAN:       "[banka hesap numarası gizlendi]",
			domain.IdentifierCard:       "[kart numarası gizlendi]",
		},
		noun: map[domain.IdentifierKind]string{
			domain.IdentifierNationalID: "kimlik numarası",
			domain.IdentifierPhone:      "telefon numarası",
			domain.IdentifierEmail:      "e-posta adresi",
			domain.IdentifierIBAN:       "banka hesap numarası",
			domain.IdentifierCard:       "kart numarası",
		},
		line: "Bu yanıtta gösterilmeyen kişisel veriler: %s. Görmeye yetkili olanlar için kaynak kayıtlarda yer alıyor.",
	},
}

// heldBack counts what the guard masked, by category.
type heldBack map[domain.IdentifierKind]int

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
	ids := identifiers.Find(reply)
	if len(ids) == 0 {
		return reply, nil
	}
	supplied := map[string]bool{}
	for _, t := range userText {
		for _, id := range identifiers.Find(t) {
			supplied[identifiers.Normalise(id.Value)] = true
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
		if supplied[identifiers.Normalise(id.Value)] {
			continue
		}
		mask := inWithholding(id.Start) || (profile == domain.ProfileCustomer && strongIdentifier(id.Kind))
		if !mask {
			continue
		}
		b.WriteString(reply[last:id.Start])
		b.WriteString(cat.label[id.Kind])
		last = id.End
		held[id.Kind]++
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
