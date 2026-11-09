package replycheck

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// Entry is one ledger entry of the turn's evidence.
type Entry struct {
	// ID is the full ledger id (led_…).
	ID string
	// Kind is the ledger kind; step outputs are model text and never back a value.
	Kind string
	Tool string
	Body string
}

// Value is one value the kernel accepted for a checklist item (its evidence_ref
// names the ledger entry it came from).
type Value struct {
	Name        string
	Type        string
	Text        string
	EvidenceRef string
}

// Item is one checklist item with its kernel-decided status.
type Item struct {
	// ID is the item id ("R1"); Run tells two delegations' "R1" apart.
	ID     string
	Run    string
	Type   string
	Status string
	Quote  string
	Text   string
	Reason string
	Values []Value
	// Evidence are the ledger ids behind the accepted values.
	Evidence []string
}

// Evidence is everything a reply may rest on.
type Evidence struct {
	Entries []Entry
	Items   []Item
	// Context is text the person supplied or the kernel stated for the turn (the
	// person's own messages, today's date block): a value restated from it is
	// backed.
	Context []string
}

// Finding is one value the reply states that the evidence does not back as cited.
type Finding struct {
	Kind    string   `json:"kind"`
	Raw     string   `json:"raw"`
	Markers []string `json:"markers,omitempty"`
	// Reason: "uncited" (no evidence backs it), "miscited" (backed, but not by the
	// entries the reply cites next to it).
	Reason string `json:"reason"`
}

// ItemGap is a checklist item the reply neither answers nor states as a gap.
type ItemGap struct {
	Item   string `json:"item"`
	Run    string `json:"run,omitempty"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Quote  string `json:"quote"`
	// Reason: "unanswered" (a satisfied item none of whose values or evidence the
	// reply states), "gap_not_stated" (a not_found / not_permitted / failed / open
	// item the reply does not tag), "unverified" (the reply carries no item tags
	// at all, so whether a gap was stated cannot be decided without reading the
	// prose — not a violation).
	Reason string `json:"reason"`
}

// Report is the result of one check.
type Report struct {
	Mentions       int       `json:"mentions"`
	Checked        int       `json:"checked"`
	Uncited        []Finding `json:"uncited,omitempty"`
	Miscited       []Finding `json:"miscited,omitempty"`
	UnknownMarkers []string  `json:"unknown_markers,omitempty"`
	Unanswered     []ItemGap `json:"unanswered,omitempty"`
	Unverified     []ItemGap `json:"unverified,omitempty"`
	// MissingSource are values the reply states from recalled memory (an
	// organisation's document: a rule, a deadline, a fee) without naming the
	// document or the article/section they come from
	// (T-20261109-chat-agent-answers-a-published-rule).
	MissingSource []SourceGap `json:"missing_source,omitempty"`
	EvidenceCount int         `json:"evidence"`
	Items         int         `json:"items"`
}

// Violations counts what the D8 check treats as violations: values no evidence
// backs, and checklist items neither answered nor stated as gaps. Miscitations,
// unknown markers and unverified gaps are reported, not counted.
func (r Report) Violations() int { return len(r.Uncited) + len(r.Unanswered) + len(r.MissingSource) }

// SourceGap is one value stated from recalled memory whose source the reply does
// not name.
type SourceGap struct {
	Raw string `json:"raw"`
	// Entry is the recall's ledger entry; Sources the document and section labels
	// the reply could have named (any one of them suffices).
	Entry   string   `json:"entry"`
	Sources []string `json:"sources"`
}

// UncitedRaw lists the uncited values as written, for the retry note.
func (r Report) UncitedRaw() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range r.Uncited {
		if !seen[f.Raw] {
			seen[f.Raw] = true
			out = append(out, f.Raw)
		}
	}
	return out
}

// ── the evidence index ───────────────────────────────────────────────────────

// index is the searchable form of a set of texts: every number reading (sorted),
// every date, and the lower-cased text for identifiers and strings.
type index struct {
	nums  []float64
	dates map[Date]bool
	md    map[[2]int]bool
	text  string
}

var evNumRe = regexp.MustCompile(`\d[\d.,]*\d|\d`)

func newIndex(texts ...string) *index {
	ix := &index{dates: map[Date]bool{}, md: map[[2]int]bool{}}
	var b strings.Builder
	for _, t := range texts {
		if t == "" {
			continue
		}
		b.WriteString(strings.ToLower(t))
		b.WriteString("\n")
		for _, tok := range evNumRe.FindAllString(t, -1) {
			for _, r := range readings(tok, 1, "") {
				ix.nums = append(ix.nums, math.Abs(r.V))
			}
			// The plain reading of a JSON-style token ("1250.5"), and every
			// dot-or-comma-free run, so "2026-11-08" also yields 2026, 11 and 8.
			if f, err := strconv.ParseFloat(strings.TrimRight(tok, ".,"), 64); err == nil {
				ix.nums = append(ix.nums, math.Abs(f))
			}
		}
		for _, d := range extractDates(t) {
			for _, dd := range d.dates {
				ix.dates[dd] = true
				ix.md[[2]int{dd.Month, dd.Day}] = true
			}
		}
	}
	sort.Float64s(ix.nums)
	ix.text = b.String()
	return ix
}

func (ix *index) hasNumber(v, tol float64) bool {
	v = math.Abs(v)
	lo, hi := v-tol-1e-9, v+tol+1e-9
	i := sort.SearchFloat64s(ix.nums, lo)
	return i < len(ix.nums) && ix.nums[i] <= hi
}

func (ix *index) hasDate(d Date) bool {
	if d.Year == 0 {
		return ix.md[[2]int{d.Month, d.Day}]
	}
	return ix.dates[d]
}

func (ix *index) hasText(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s != "" && strings.Contains(ix.text, s)
}

// backs reports whether ix holds the value m states.
func (ix *index) backs(m Mention) bool {
	switch m.Kind {
	case KindID:
		return ix.hasText(m.ID)
	case KindDate:
		for _, d := range m.Dates {
			if ix.hasDate(d) {
				return true
			}
		}
		return false
	case KindPercent:
		for _, r := range m.Values {
			if ix.hasNumber(r.V, r.Tol) || ix.hasNumber(r.V/100, r.Tol/100) {
				return true
			}
		}
		return false
	default:
		for _, r := range m.Values {
			if ix.hasNumber(r.V, r.Tol) {
				return true
			}
		}
		return false
	}
}

// citable reports whether an entry may back a value: tool results, calculations
// and artifacts — never a step output, which is model text (ADR-0145 D5), nor
// turn scratch.
func citable(kind string) bool {
	switch kind {
	case "step_output", "scratch":
		return false
	}
	return true
}

// ── the check ───────────────────────────────────────────────────────────────

// Check tests reply against ev.
func Check(reply string, ev Evidence, opt Options) Report {
	mentions := Extract(reply, opt)
	rep := Report{Mentions: len(mentions), Items: len(ev.Items)}

	var all []string
	byID := map[string]*index{}
	var ids []string
	for _, e := range ev.Entries {
		if !citable(e.Kind) {
			continue
		}
		rep.EvidenceCount++
		all = append(all, e.Body)
		ids = append(ids, e.ID)
	}
	for _, it := range ev.Items {
		for _, v := range it.Values {
			all = append(all, v.Text)
		}
	}
	all = append(all, ev.Context...)
	whole := newIndex(all...)
	entryIndex := func(id string) *index {
		if ix, ok := byID[id]; ok {
			return ix
		}
		var body []string
		for _, e := range ev.Entries {
			if e.ID == id {
				body = append(body, e.Body)
			}
		}
		for _, it := range ev.Items {
			for _, v := range it.Values {
				if v.EvidenceRef == id {
					body = append(body, v.Text)
				}
			}
		}
		ix := newIndex(body...)
		byID[id] = ix
		return ix
	}
	resolve := func(marker string) (string, bool) {
		for _, id := range ids {
			if strings.HasPrefix(id, marker) {
				return id, true
			}
		}
		for _, it := range ev.Items {
			for _, id := range it.Evidence {
				if strings.HasPrefix(id, marker) {
					return id, true
				}
			}
		}
		return "", false
	}

	unknown := map[string]bool{}
	for _, m := range mentions {
		rep.Checked++
		var ledgerMarks []string
		for _, mk := range m.Markers {
			if strings.HasPrefix(mk, "led_") {
				ledgerMarks = append(ledgerMarks, mk)
			}
		}
		if !whole.backs(m) {
			rep.Uncited = append(rep.Uncited, Finding{Kind: m.Kind, Raw: m.Raw, Markers: m.Markers, Reason: "uncited"})
			continue
		}
		if len(ledgerMarks) == 0 {
			continue
		}
		cited := false
		for _, mk := range ledgerMarks {
			id, ok := resolve(mk)
			if !ok {
				unknown[mk] = true
				continue
			}
			if entryIndex(id).backs(m) {
				cited = true
				break
			}
		}
		if !cited {
			rep.Miscited = append(rep.Miscited, Finding{Kind: m.Kind, Raw: m.Raw, Markers: m.Markers, Reason: "miscited"})
		}
	}
	for _, mk := range Markers(reply) {
		if strings.HasPrefix(mk, "led_") {
			if _, ok := resolve(mk); !ok {
				unknown[mk] = true
			}
		}
	}
	for mk := range unknown {
		rep.UnknownMarkers = append(rep.UnknownMarkers, mk)
	}
	sort.Strings(rep.UnknownMarkers)

	rep.Unanswered, rep.Unverified = checkItems(reply, mentions, ev.Items)
	rep.MissingSource = checkSources(reply, mentions, ev)
	return rep
}

// ── sources of recalled organisation facts ──────────────────────────────────

// recallBody is the body of a memory recall's ledger entry
// (network/recall_ledger.go): the recalled chunks with their source document and
// section.
type recallBody struct {
	Hits []struct {
		Text     string `json:"text"`
		Document string `json:"document"`
		Section  string `json:"section"`
	} `json:"hits"`
}

// headingRe finds a numbered heading in a recalled chunk ("**4. Time for
// compliance**", "MADDE 4 – Cevap süresi", "Section 12 Fees"): an optional word,
// a number, and the title after it, on its own line.
var headingRe = regexp.MustCompile(`(?m)^[\s*#_]*((?:[\p{L}§]+\.?\s+)?\d+(?:\.\d+)*[.)]?(?:\s*[–—:-])?\s*[^\n*]{0,80}?)[\s*_]*$`)

// sourceBlock is one part of a recalled chunk a reply may name: the text under
// one heading (or the whole chunk when it has none) with its labels.
type sourceBlock struct {
	ix     *index
	labels []string
}

// sourceBlocks splits a recalled chunk at its numbered headings. Each block's
// labels are the chunk's document and section path plus its own heading (whole,
// the designator with its number such as "MADDE 4", and a title of two words or
// more), so a value is tied to the heading it sits under.
func sourceBlocks(document, section, text string) []sourceBlock {
	var base []string
	add := func(dst *[]string, l string) {
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "*_#:–—-."))
		if len([]rune(l)) >= 4 {
			*dst = appendNew(*dst, l)
		}
	}
	add(&base, document)
	for _, seg := range strings.Split(section, ">") {
		addHeading(func(l string) { add(&base, l) }, seg)
	}
	locs := headingRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return []sourceBlock{{ix: newIndex(text), labels: base}}
	}
	var out []sourceBlock
	if pre := strings.TrimSpace(text[:locs[0][0]]); pre != "" {
		out = append(out, sourceBlock{ix: newIndex(pre), labels: base})
	}
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		labels := append([]string(nil), base...)
		addHeading(func(l string) { add(&labels, l) }, text[loc[2]:loc[3]])
		out = append(out, sourceBlock{ix: newIndex(text[loc[0]:end]), labels: labels})
	}
	return out
}

// sourceLabels are every label of a recalled chunk (all its blocks).
func sourceLabels(document, section, text string) []string {
	var out []string
	for _, b := range sourceBlocks(document, section, text) {
		out = appendNew(out, b.labels...)
	}
	return out
}

var designatorRe = regexp.MustCompile(`^\s*((?:[\p{L}§]+\.?\s+)?\d+(?:\.\d+)*)[.)]?\s*[–—:-]?\s*(.*)$`)

func addHeading(add func(string), h string) {
	add(h)
	if m := designatorRe.FindStringSubmatch(strings.TrimSpace(h)); m != nil {
		if strings.IndexFunc(m[1], unicode.IsLetter) >= 0 {
			add(m[1]) // "MADDE 4", "Section 12": a designator with its word
		}
		if len(strings.Fields(m[2])) >= 2 {
			add(m[2]) // "Time for compliance", "Cevap süresi"; one word is too common to count
		}
	}
}

// checkSources reports the values the reply states that ONLY recalled memory
// backs (not a tool result, a checklist value or the person's own words) when the
// reply names none of the recalled chunk's source labels (D8 + the grounded-reply
// rule: an organisation's rule or figure names its document and article/section).
func checkSources(reply string, mentions []Mention, ev Evidence) []SourceGap {
	type hit struct {
		entry  string
		ix     *index
		labels []string
	}

	var hits []hit
	var other []string
	for _, e := range ev.Entries {
		if !citable(e.Kind) {
			continue
		}
		if e.Tool != domain.RecallToolName {
			other = append(other, e.Body)
			continue
		}
		var b recallBody
		if json.Unmarshal([]byte(e.Body), &b) != nil {
			other = append(other, e.Body)
			continue
		}
		for _, h := range b.Hits {
			for _, blk := range sourceBlocks(h.Document, h.Section, h.Text) {
				hits = append(hits, hit{entry: e.ID, ix: blk.ix, labels: blk.labels})
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	for _, it := range ev.Items {
		for _, v := range it.Values {
			other = append(other, v.Text)
		}
	}
	other = append(other, ev.Context...)
	nonRecall := newIndex(other...)
	lower := strings.ToLower(StripMarkers(reply))
	var out []SourceGap
	seen := map[string]bool{}
	for _, m := range mentions {
		if nonRecall.backs(m) {
			continue
		}
		var gap *SourceGap
		named := false
		for _, h := range hits {
			if len(h.labels) == 0 || !h.ix.backs(m) {
				continue
			}
			for _, l := range h.labels {
				if strings.Contains(lower, strings.ToLower(l)) {
					named = true
					break
				}
			}
			if named {
				break
			}
			if gap == nil {
				gap = &SourceGap{Raw: m.Raw, Entry: h.entry}
			}
			gap.Sources = appendNew(gap.Sources, h.labels...)
		}
		if named || gap == nil || seen[m.Raw] {
			continue
		}
		seen[m.Raw] = true
		out = append(out, *gap)
	}
	return out
}

func appendNew(xs []string, ys ...string) []string {
	for _, y := range ys {
		dup := false
		for _, x := range xs {
			if x == y {
				dup = true
				break
			}
		}
		if !dup {
			xs = append(xs, y)
		}
	}
	return xs
}

// checkItems decides, per checklist item, whether the reply answers it or states
// it as a gap (D8: "every checklist item is answered or stated as not found or
// not permitted"). A constraint is a limit on the work, not something to answer.
func checkItems(reply string, mentions []Mention, items []Item) (unanswered, unverified []ItemGap) {
	tags := map[string]bool{}
	ledgerMarks := map[string]bool{}
	for _, mk := range Markers(reply) {
		if strings.HasPrefix(mk, "R") {
			tags[mk] = true
		} else {
			ledgerMarks[mk] = true
		}
	}
	lower := strings.ToLower(StripMarkers(reply))
	replyIx := mentionIndex(mentions)
	for _, it := range items {
		if it.Type == "constraint" {
			continue
		}
		gap := ItemGap{Item: it.ID, Run: it.Run, Type: it.Type, Status: it.Status, Quote: it.Quote}
		if it.Status == "satisfied" {
			if tags[it.ID] || itemAnswered(it, lower, replyIx, ledgerMarks) {
				continue
			}
			gap.Reason = "unanswered"
			unanswered = append(unanswered, gap)
			continue
		}
		// not_found / not_permitted / failed, or still open: a gap to state.
		if tags[it.ID] {
			continue
		}
		if len(tags) == 0 {
			gap.Reason = "unverified"
			unverified = append(unverified, gap)
			continue
		}
		gap.Reason = "gap_not_stated"
		unanswered = append(unanswered, gap)
	}
	return unanswered, unverified
}

// mentionIndex is the reply's own values, for matching an item's accepted
// values against what the reply states (by value, so "£20,092.11" answers a
// money value 20092.11 and "8 Kasım 2026" a date 2026-11-08).
type replyValues struct {
	nums  []Reading
	dates []Date
}

func mentionIndex(ms []Mention) replyValues {
	var rv replyValues
	for _, m := range ms {
		switch m.Kind {
		case KindDate:
			rv.dates = append(rv.dates, m.Dates...)
		case KindID:
		default:
			rv.nums = append(rv.nums, m.Values...)
		}
	}
	return rv
}

func itemAnswered(it Item, lowerReply string, rv replyValues, ledgerMarks map[string]bool) bool {
	for _, ref := range it.Evidence {
		for mk := range ledgerMarks {
			if strings.HasPrefix(ref, mk) {
				return true
			}
		}
	}
	for _, v := range it.Values {
		if valueStated(v, lowerReply, rv) {
			return true
		}
	}
	return false
}

func valueStated(v Value, lowerReply string, rv replyValues) bool {
	t := strings.TrimSpace(v.Text)
	if t == "" {
		return false
	}
	switch v.Type {
	case "number", "money":
		want, ok := parseValueNumber(t)
		if !ok {
			return false
		}
		for _, r := range rv.nums {
			if math.Abs(math.Abs(r.V)-math.Abs(want)) <= r.Tol+1e-9 {
				return true
			}
		}
		return false
	case "date":
		ds := extractDates(t)
		for _, d := range ds {
			for _, want := range d.dates {
				for _, got := range rv.dates {
					if got.Month == want.Month && got.Day == want.Day && (got.Year == 0 || got.Year == want.Year) {
						return true
					}
				}
			}
		}
		return false
	case "list", "object":
		// Any scalar leaf of a structured value stated in the reply answers it.
		for _, leaf := range scalarLeaves(t) {
			if len(leaf) >= 3 && strings.Contains(lowerReply, strings.ToLower(leaf)) {
				return true
			}
		}
		return false
	case "bool":
		return false
	}
	return strings.Contains(lowerReply, strings.ToLower(t))
}

func parseValueNumber(t string) (float64, bool) {
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, true
	}
	rs := readings(strings.TrimSpace(evNumRe.FindString(t)), 1, "")
	if len(rs) == 0 {
		return 0, false
	}
	return rs[0].V, true
}

var leafRe = regexp.MustCompile(`"([^"\\]{3,200})"|(-?\d+(?:\.\d+)?)`)

func scalarLeaves(jsonText string) []string {
	var out []string
	for _, m := range leafRe.FindAllStringSubmatch(jsonText, -1) {
		if m[1] != "" {
			out = append(out, m[1])
		} else if m[2] != "" {
			out = append(out, m[2])
		}
	}
	return out
}
