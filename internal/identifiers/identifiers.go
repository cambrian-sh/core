// Package identifiers is the one deterministic personal-identifier detector
// (domain.IdentifierDetector). The chat reply disclosure guard
// (internal/chat/disclosure.go, T-20261108-records-request-replies-still-restate-private)
// and the executor's outbound identifier guard (domain/outbound_identifiers.go,
// T-20261108-outbound-tool-writes-have-no-deterministic) both use it, so a shape
// recognised in a reply is recognised in a tool argument and the two never drift.
//
// It recognises e-mail addresses, IBANs (ISO 13616 mod-97), UK National
// Insurance and US Social Security numbers, payment card numbers (Luhn), eleven-
// digit national ID numbers and phone numbers written the way phone numbers are
// written. It does NOT recognise names: a private person's name has no shape a
// deterministic check can tell from other capitalised words.
package identifiers

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// Detector implements domain.IdentifierDetector.
type Detector struct{}

var _ domain.IdentifierDetector = Detector{}

// FindIdentifiers implements domain.IdentifierDetector.
func (Detector) FindIdentifiers(text string) []domain.IdentifierMatch { return Find(text) }

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

// Find returns the identifiers in text, left to right, without overlaps.
func Find(text string) []domain.IdentifierMatch {
	var found []domain.IdentifierMatch
	add := func(start, end int, kind domain.IdentifierKind) {
		for _, f := range found {
			if start < f.End && f.Start < end {
				return // an earlier, more specific shape already holds the span
			}
		}
		found = append(found, domain.IdentifierMatch{Start: start, End: end, Kind: kind, Value: text[start:end]})
	}
	each := func(re *regexp.Regexp, classify func(string) (domain.IdentifierKind, bool)) {
		for _, m := range re.FindAllStringIndex(text, -1) {
			if !freeStanding(text, m[0], m[1]) {
				continue // part of a reference number, a date, an amount or a code
			}
			if kind, ok := classify(text[m[0]:m[1]]); ok {
				add(m[0], m[1], kind)
			}
		}
	}
	each(reEmail, func(string) (domain.IdentifierKind, bool) { return domain.IdentifierEmail, true })
	each(reIBAN, func(s string) (domain.IdentifierKind, bool) { return domain.IdentifierIBAN, validIBAN(s) })
	each(reNINO, func(string) (domain.IdentifierKind, bool) { return domain.IdentifierNationalID, true })
	each(reSSN, func(string) (domain.IdentifierKind, bool) { return domain.IdentifierNationalID, true })
	each(reCard, func(s string) (domain.IdentifierKind, bool) { return domain.IdentifierCard, luhnValid(onlyDigits(s)) })
	each(rePlainDigits, classifyPlain)
	each(rePhone, classifyPhone)
	sort.Slice(found, func(i, j int) bool { return found[i].Start < found[j].Start })
	return found
}

// Normalise is the comparison form of an identifier: digits and letters only,
// upper-cased, so "0 (509) 735 17 39" and "05097351739" are the same number.
func Normalise(s string) string {
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
func classifyPlain(s string) (domain.IdentifierKind, bool) {
	switch n := len(s); {
	case n == 11:
		return domain.IdentifierNationalID, true
	case n >= 13 && n <= 19 && luhnValid(s):
		return domain.IdentifierCard, true
	}
	return "", false
}

// classifyPhone accepts a grouped number of phone length (10-15 digits) written the way
// phone numbers are written: with an international "+", a trunk "0", an area code in
// parentheses, or the North American 3-3-4 grouping. That keeps a list of years ("2024 2025
// 2026") or of page numbers from reading as a phone number. Dates ("24.09.2026", 8 digits)
// are too short; a dot-grouped amount ("1.234.567.890", every group after the first three
// digits long) is an amount.
func classifyPhone(s string) (domain.IdentifierKind, bool) {
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
	return domain.IdentifierPhone, true
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
