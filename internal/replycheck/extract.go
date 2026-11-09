// Package replycheck is the ADR-0145 D8 reply check: a deterministic test, with no
// model call, of a chat reply against the evidence the turn produced
// (T-20261108-chat-replies-state-figures-dates-and-ids-no).
//
// It extracts the figures, money amounts, percentages, dates and identifiers a
// reply states, in English and Turkish formats ("£1.4M", "1.234,56 TL", "%12,5",
// "8 Kasım 2026", "08.11.2026", "November 8, 2026"), and asks whether each one is
// backed by a value in the evidence: the turn's ledger entries (tool results,
// calculations, artifacts — never a model-written step output, ADR-0145 D5), the
// values the kernel accepted for the request's checklist items, and the person's
// own words. Numbers compare by value with the precision the reply wrote them in,
// so a rounded figure ("£1.4M", "66,484") is backed by the exact one it rounds.
// It also asks whether every checklist item is answered (one of its accepted values,
// or its evidence, appears in the reply) or stated as a gap.
//
// The package is pure: no I/O, no clock, no model. internal/chat gathers the
// evidence through the enforced ledger reader and decides what to do with the
// report (log and count it; when enforced, one retry naming the uncited values).
package replycheck

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Mention kinds.
const (
	KindMoney   = "money"
	KindNumber  = "number"
	KindPercent = "percent"
	KindDate    = "date"
	KindID      = "id"
)

// Date is a calendar day; Year 0 means the text named no year.
type Date struct {
	Year, Month, Day int
}

// Mention is one checkable value the reply states.
type Mention struct {
	Kind string `json:"kind"`
	// Raw is the text as written.
	Raw string `json:"raw"`
	// Values are the readings of a number (an English and a Turkish reading of
	// "1.234" differ); each pairs with the tolerance its written precision implies.
	Values []Reading `json:"-"`
	// Dates are the readings of a date ("03/04/2026" is 3 April or 4 March).
	Dates []Date `json:"-"`
	// ID is the identifier, for KindID.
	ID string `json:"-"`
	// Markers are the evidence markers cited right after the value, in the same
	// sentence: ledger id prefixes ("led_3fa9c2d1") and item tags ("R2").
	Markers []string `json:"markers,omitempty"`
	// Offset is the byte offset of Raw in the reply.
	Offset int `json:"-"`
}

// Reading is one numeric interpretation with the tolerance its precision implies
// (half a unit of the last written digit, times the magnitude suffix).
type Reading struct {
	V, Tol float64
}

// Options tunes extraction.
type Options struct {
	// MinInteger: a bare integer (no currency, no decimals, no grouping, no
	// percent) below this is not checked — counts and list positions ("3
	// invoices", "step 2") are rarely stated in evidence. 0 checks every integer.
	MinInteger int
	// Lang is the reply's language ("en", "tr"); it orders the readings of an
	// ambiguous number, both are still tried.
	Lang string
}

// ── markers ────────────────────────────────────────────────────────────────

// markerRe matches one bracketed citation group: [led_3fa9c2d1], [R2],
// [led_3fa9c2d1, R2]. Ledger ids are cited by a prefix of at least 6 hex digits.
var markerRe = regexp.MustCompile(`\[\s*(?:led_[0-9a-fA-F]{6,32}|R\d{1,3})(?:\s*[,;]\s*(?:led_[0-9a-fA-F]{6,32}|R\d{1,3}))*\s*\]`)
var markerTokRe = regexp.MustCompile(`led_[0-9a-fA-F]{6,32}|R\d{1,3}`)

type markerSpan struct {
	start, end int
	toks       []string
}

func findMarkers(text string) []markerSpan {
	var out []markerSpan
	for _, loc := range markerRe.FindAllStringIndex(text, -1) {
		toks := markerTokRe.FindAllString(text[loc[0]:loc[1]], -1)
		for i, t := range toks {
			toks[i] = strings.ToLower(t)
			if strings.HasPrefix(toks[i], "r") {
				toks[i] = strings.ToUpper(toks[i])
			}
		}
		out = append(out, markerSpan{start: loc[0], end: loc[1], toks: toks})
	}
	return out
}

// Markers returns every marker token the reply cites, in order, deduplicated.
func Markers(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range findMarkers(text) {
		for _, t := range m.toks {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

var spaceBeforePunct = regexp.MustCompile(`[ \t]+([.,;:!?)])`)
var doubleSpace = regexp.MustCompile(`[ \t]{2,}`)

// StripMarkers removes the evidence markers from a reply before it is shown to
// the person (the references stay in the turn index and the ledger).
func StripMarkers(text string) string {
	if !markerRe.MatchString(text) {
		return text
	}
	out := markerRe.ReplaceAllString(text, "")
	out = spaceBeforePunct.ReplaceAllString(out, "$1")
	out = doubleSpace.ReplaceAllString(out, " ")
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ── extraction ─────────────────────────────────────────────────────────────

var monthNames = map[string]int{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3, "april": 4, "apr": 4,
	"may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7, "august": 8, "aug": 8,
	"september": 9, "sep": 9, "sept": 9, "october": 10, "oct": 10, "november": 11, "nov": 11,
	"december": 12, "dec": 12,
	// Turkish (both dotted and undotted spellings models produce).
	"ocak": 1, "şubat": 2, "subat": 2, "mart": 3, "nisan": 4, "mayıs": 5, "mayis": 5,
	"haziran": 6, "temmuz": 7, "ağustos": 8, "agustos": 8, "eylül": 9, "eylul": 9,
	"ekim": 10, "kasım": 11, "kasim": 11, "aralık": 12, "aralik": 12,
}

var (
	isoDateRe = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})(?:[T ][0-9:.]+(?:Z|[+-]\d{2}:?\d{2})?)?`)
	numDateRe = regexp.MustCompile(`\b(\d{1,2})([./])(\d{1,2})([./])(\d{4}|\d{2})\b`)
	// "8 November 2026", "8th Nov", "8 Kasım 2026'da". The month is any word,
	// looked up in monthNames: Go's \b is ASCII-only, so "Kasım" cannot be
	// bounded by it.
	dayMonthRe = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th|\.)?\s+(\p{L}+)\.?(?:,?\s+(\d{4}))?`)
	// "November 8, 2026", "Nov 8"; the day must not run on into a year
	// ("November 2026" names no day).
	monthDayRe = regexp.MustCompile(`(\p{L}+)\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s+(\d{4}))?(?:[^\d]|$)`)
	timeRe     = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?(?:\s?[aApP][mM])?\b`)

	// Identifiers: a letter-led token with a digit and a separator (CUS-004,
	// INV-2026/0012, MSG-000173), or capitals followed by at least three digits
	// (AB123456).
	idSepRe   = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:[-_/][A-Za-z0-9]+)+\b`)
	idPlainRe = regexp.MustCompile(`\b[A-Z]{2,}\d{3,}[A-Z]?\b`)
	emailRe   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	urlRe     = regexp.MustCompile(`https?://\S+`)

	numTok  = `\d{1,3}(?:[.,'\x{00A0}]\d{3})+(?:[.,]\d+)?|\d+(?:[.,]\d+)?`
	scaleRe = `k|K|m|M|mn|bn|million|billion|thousand|milyon|milyar|bin`
	curPre  = `£|\$|€|₺|\b(?:GBP|USD|EUR|TRY|TL)\b`
	curPost = `£|\$|€|₺|\b(?:GBP|USD|EUR|TRY|TL|lira|liras|pounds?|euros?|dollars?)\b`

	moneyPreRe  = regexp.MustCompile(`(?:` + curPre + `)\s?(-?(?:` + numTok + `))(?:\s?(` + scaleRe + `)\b)?`)
	moneyPostRe = regexp.MustCompile(`(-?(?:` + numTok + `))(?:\s?(` + scaleRe + `)\b)?\s?(?:` + curPost + `)`)
	pctPreRe    = regexp.MustCompile(`(?i)(?:%|\byüzde\s)\s?(` + numTok + `)`)
	pctPostRe   = regexp.MustCompile(`(?i)(` + numTok + `)\s?(?:%|\bpercent\b|\bper cent\b)`)
	numberRe    = regexp.MustCompile(`(?:^|[^\w.,])(-?(?:` + numTok + `))(?:\s?(` + scaleRe + `)\b)?`)
	listItemRe  = regexp.MustCompile(`(?m)^[ \t]*(?:[-*][ \t]+)?\d{1,3}[.)][ \t]`)
	ordinalRe   = regexp.MustCompile(`\b\d+(?:st|nd|rd|th)\b`)
)

// masker blanks spans already claimed by an earlier extractor, keeping offsets.
type masker struct{ b []byte }

func (m *masker) blank(start, end int) {
	for i := start; i < end && i < len(m.b); i++ {
		if m.b[i] != '\n' {
			m.b[i] = ' '
		}
	}
}
func (m *masker) String() string { return string(m.b) }

// Extract returns the checkable values text states, in order of appearance.
func Extract(text string, opt Options) []Mention {
	marks := findMarkers(text)
	m := &masker{b: []byte(text)}
	for _, mk := range marks {
		m.blank(mk.start, mk.end)
	}
	for _, re := range []*regexp.Regexp{urlRe, emailRe} {
		for _, loc := range re.FindAllStringIndex(m.String(), -1) {
			m.blank(loc[0], loc[1])
		}
	}
	var out []Mention
	add := func(mn Mention, start, end int) {
		mn.Offset = start
		mn.Raw = strings.TrimSpace(text[start:end])
		mn.Markers = markersAfter(text, marks, end)
		out = append(out, mn)
		m.blank(start, end)
	}

	// Dates first: their digits must not be read as numbers.
	for _, d := range extractDates(m.String()) {
		add(Mention{Kind: KindDate, Dates: d.dates}, d.start, d.end)
	}
	for _, loc := range timeRe.FindAllStringIndex(m.String(), -1) {
		m.blank(loc[0], loc[1])
	}
	// Identifiers next (CUS-004's digits are not a number).
	for _, re := range []*regexp.Regexp{idSepRe, idPlainRe} {
		for _, loc := range re.FindAllStringIndex(m.String(), -1) {
			tok := m.String()[loc[0]:loc[1]]
			if !hasDigit(tok) || !hasLetter(tok) {
				continue
			}
			add(Mention{Kind: KindID, ID: tok}, loc[0], loc[1])
		}
	}
	// Money: a currency on either side; the magnitude suffix scales it.
	for _, re := range []*regexp.Regexp{moneyPreRe, moneyPostRe} {
		for _, sm := range re.FindAllStringSubmatchIndex(m.String(), -1) {
			s := m.String()
			num := s[sm[2]:sm[3]]
			scale := ""
			if sm[4] >= 0 {
				scale = s[sm[4]:sm[5]]
			}
			rs := readings(num, scaleOf(scale), opt.Lang)
			if len(rs) == 0 {
				continue
			}
			add(Mention{Kind: KindMoney, Values: rs}, sm[0], sm[1])
		}
	}
	for _, re := range []*regexp.Regexp{pctPreRe, pctPostRe} {
		for _, sm := range re.FindAllStringSubmatchIndex(m.String(), -1) {
			rs := readings(m.String()[sm[2]:sm[3]], 1, opt.Lang)
			if len(rs) == 0 {
				continue
			}
			add(Mention{Kind: KindPercent, Values: rs}, sm[0], sm[1])
		}
	}
	// List positions and ordinals are not figures.
	for _, re := range []*regexp.Regexp{listItemRe, ordinalRe} {
		for _, loc := range re.FindAllStringIndex(m.String(), -1) {
			m.blank(loc[0], loc[1])
		}
	}
	for _, sm := range numberRe.FindAllStringSubmatchIndex(m.String(), -1) {
		s := m.String()
		num := s[sm[2]:sm[3]]
		scale := ""
		end := sm[3]
		if sm[4] >= 0 {
			scale = s[sm[4]:sm[5]]
			end = sm[5]
		}
		// "4." closing a Turkish ordinal ("4. Madde") and a trailing sentence dot
		// are not decimals.
		num = strings.TrimRight(num, ".,")
		if num == "" || num == "-" {
			continue
		}
		if scale == "" && isBareInteger(num) {
			if v, err := strconv.Atoi(strings.TrimPrefix(num, "-")); err == nil && v < opt.MinInteger {
				m.blank(sm[2], end)
				continue
			}
		}
		rs := readings(num, scaleOf(scale), opt.Lang)
		if len(rs) == 0 {
			continue
		}
		add(Mention{Kind: KindNumber, Values: rs}, sm[2], sm[2]+len(num)+(end-sm[3]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// markersAfter returns the markers cited after end within the same sentence (up
// to the next sentence end or line break), so "£20,092.11 [led_3fa9c2d1]" and
// "... was £20,092.11 and is overdue [led_3fa9c2d1]." both cite the entry.
func markersAfter(text string, marks []markerSpan, end int) []string {
	var out []string
	limit := len(text)
	for i := end; i < len(text); i++ {
		c := text[i]
		if c == '\n' || ((c == '.' || c == '!' || c == '?') && (i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\n')) {
			limit = i
			break
		}
	}
	for _, mk := range marks {
		if mk.start >= end && mk.start <= limit+1 {
			out = append(out, mk.toks...)
		}
	}
	return out
}

func hasDigit(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0
}
func hasLetter(s string) bool {
	return strings.ContainsAny(strings.ToLower(s), "abcdefghijklmnopqrstuvwxyz")
}

func isBareInteger(num string) bool {
	n := strings.TrimPrefix(num, "-")
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return n != ""
}

func scaleOf(s string) float64 {
	switch strings.ToLower(s) {
	case "k", "thousand", "bin":
		return 1e3
	case "m", "mn", "million", "milyon":
		return 1e6
	case "bn", "billion", "milyar":
		return 1e9
	}
	return 1
}

// readings returns every valid reading of a number token, the language's own
// first. "1.234" is 1.234 in English and 1234 in Turkish; "12,5" is 12.5 (a
// three-digit group would make "1,250" 1250); "1.234,56" and "1,234.56" are
// unambiguous. Each reading's tolerance is half a unit of its last digit.
func readings(tok string, scale float64, lang string) []Reading {
	tok = strings.NewReplacer("'", "", " ", "", " ", "").Replace(strings.TrimSpace(tok))
	neg := strings.HasPrefix(tok, "-")
	tok = strings.TrimPrefix(tok, "-")
	if tok == "" {
		return nil
	}
	type cand struct{ intPart, frac string }
	var cands []cand
	dot, comma := strings.Count(tok, "."), strings.Count(tok, ",")
	split := func(dec string, group string) (cand, bool) {
		t := tok
		// A group mark after the decimal mark is not this convention
		// ("5,971.75" has no Turkish reading).
		if di := strings.Index(t, dec); di >= 0 && strings.Contains(t[di:], group) {
			return cand{}, false
		}
		if group != "" {
			parts := strings.Split(strings.SplitN(t, dec, 2)[0], group)
			for i, p := range parts {
				if i > 0 && len(p) != 3 {
					return cand{}, false
				}
				if i == 0 && (len(p) == 0 || len(p) > 3) && len(parts) > 1 {
					return cand{}, false
				}
			}
			t = strings.ReplaceAll(t, group, "")
		}
		if dec != "" && strings.Contains(t, dec) {
			ps := strings.SplitN(t, dec, 2)
			if strings.Contains(ps[1], dec) {
				return cand{}, false
			}
			return cand{ps[0], ps[1]}, true
		}
		return cand{t, ""}, true
	}
	en := func() { // "," groups, "." decimal
		if c, ok := split(".", ","); ok && strings.Count(tok, ".") <= 1 {
			cands = append(cands, c)
		}
	}
	tr := func() { // "." groups, "," decimal
		if c, ok := split(",", "."); ok && strings.Count(tok, ",") <= 1 {
			cands = append(cands, c)
		}
	}
	switch {
	case dot == 0 && comma == 0:
		cands = append(cands, cand{tok, ""})
	case strings.HasPrefix(strings.ToLower(lang), "tr"):
		tr()
		en()
	default:
		en()
		tr()
	}
	var out []Reading
	seen := map[float64]bool{}
	for _, c := range cands {
		if c.intPart == "" {
			c.intPart = "0"
		}
		v, err := strconv.ParseFloat(c.intPart+"."+c.frac+"0", 64)
		if err != nil {
			continue
		}
		tol := 0.5 * math.Pow(10, -float64(len(c.frac)))
		if neg {
			v = -v
		}
		v *= scale
		tol *= scale
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, Reading{V: v, Tol: tol})
	}
	return out
}

type dateSpan struct {
	start, end int
	dates      []Date
}

func validDate(d Date) bool {
	return d.Month >= 1 && d.Month <= 12 && d.Day >= 1 && d.Day <= 31
}

// extractDates finds every date text states, with every reading of an
// ambiguous numeric one (day-month first: the Turkish, British and ISO-adjacent
// order; month-day as the alternative).
func extractDates(text string) []dateSpan {
	var out []dateSpan
	taken := make([]bool, len(text)+1)
	claim := func(s, e int, ds []Date) {
		for i := s; i < e; i++ {
			if taken[i] {
				return
			}
		}
		var valid []Date
		for _, d := range ds {
			if validDate(d) {
				valid = append(valid, d)
			}
		}
		if len(valid) == 0 {
			return
		}
		for i := s; i < e; i++ {
			taken[i] = true
		}
		out = append(out, dateSpan{start: s, end: e, dates: valid})
	}
	for _, sm := range isoDateRe.FindAllStringSubmatchIndex(text, -1) {
		y, _ := strconv.Atoi(text[sm[2]:sm[3]])
		mo, _ := strconv.Atoi(text[sm[4]:sm[5]])
		d, _ := strconv.Atoi(text[sm[6]:sm[7]])
		claim(sm[0], sm[1], []Date{{y, mo, d}})
	}
	for _, sm := range numDateRe.FindAllStringSubmatchIndex(text, -1) {
		if text[sm[4]:sm[5]] != text[sm[8]:sm[9]] { // mixed separators: not a date
			continue
		}
		a, _ := strconv.Atoi(text[sm[2]:sm[3]])
		b, _ := strconv.Atoi(text[sm[6]:sm[7]])
		y, _ := strconv.Atoi(text[sm[10]:sm[11]])
		if y < 100 {
			y += 2000
		}
		ds := []Date{{y, b, a}}
		if a != b && a <= 12 {
			ds = append(ds, Date{y, a, b})
		}
		claim(sm[0], sm[1], ds)
	}
	for _, sm := range dayMonthRe.FindAllStringSubmatchIndex(text, -1) {
		d, _ := strconv.Atoi(text[sm[2]:sm[3]])
		mo, ok := monthOf(text[sm[4]:sm[5]], sm[6] >= 0)
		if !ok {
			continue
		}
		y := 0
		end := sm[5]
		if sm[6] >= 0 {
			y, _ = strconv.Atoi(text[sm[6]:sm[7]])
			end = sm[7]
		}
		claim(sm[0], end, []Date{{y, mo, d}})
	}
	for _, sm := range monthDayRe.FindAllStringSubmatchIndex(text, -1) {
		if sm[2] > 0 && isWordByte(text[sm[2]-1]) {
			continue // the month word is the tail of a longer word
		}
		mo, ok := monthOf(text[sm[2]:sm[3]], sm[6] >= 0)
		if !ok {
			continue
		}
		d, _ := strconv.Atoi(text[sm[4]:sm[5]])
		y := 0
		end := sm[5]
		if sm[6] >= 0 {
			y, _ = strconv.Atoi(text[sm[6]:sm[7]])
			end = sm[7]
		}
		claim(sm[2], end, []Date{{y, mo, d}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// monthOf reads a month word. The English "may" (and "mar", "jun", "jul" and
// "sep" in lower case) is also an ordinary word, so without a year it counts
// only capitalised ("20 May", not "20 may apply").
func monthOf(word string, hasYear bool) (int, bool) {
	mo, ok := monthNames[lowerTR(word)]
	if !ok {
		return 0, false
	}
	if !hasYear && word == strings.ToLower(word) {
		switch word {
		case "may", "mar", "jun", "jul", "sep":
			return 0, false
		}
	}
	return mo, true
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// lowerTR lowercases with the Turkish dotted/dotless I mapping, so "KASIM" and
// "Kasım" both find the month.
func lowerTR(s string) string {
	s = strings.NewReplacer("I", "ı", "İ", "i").Replace(s)
	s = strings.ToLower(s)
	if _, ok := monthNames[s]; ok {
		return s
	}
	// An English month typed in capitals ("NOVEMBER") went through the Turkish
	// mapping above; try the plain lowering too.
	return strings.ToLower(strings.NewReplacer("ı", "i").Replace(s))
}
