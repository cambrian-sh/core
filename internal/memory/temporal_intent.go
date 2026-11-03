package memory

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	dps "github.com/markusmobius/go-dateparser"
	"github.com/markusmobius/go-dateparser/date"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 D3: query-conditioned time.
//
// A question carries two different kinds of time information, and Stage A used to
// apply neither (investigation 2026-11-03-ranking-redesign F3): every query got the
// same recency prior whether it asked about "two weeks ago", "the first time", or
// "what is it now". TemporalIntent reads both out of the query text:
//
//   - a TARGET band ("two weeks ago" → about a week centred on reference_time − 14 d),
//     which becomes the temporal-fit feature exp(-(Δ/σ)²); and
//   - an ORDER ("most recently", "the first time", "currently", "how has it changed"),
//     which sets the recency direction.
//
// Date expressions are parsed by github.com/markusmobius/go-dateparser (BSD-3, a
// port of Python dateparser) — decisions/use-parser-libraries-not-hand-rolled.md.
// What this file adds around the library is small and listed so it can be audited:
// quantifier normalisation the library lacks ("a couple of days ago"), rejection of
// matches that are not time expressions in a question ("ago", "now"), the band a
// matched phrase denotes, and the order lexicon (order words are not dates, so no
// date parser recognises them).
//
// No LLM is involved and the function is pure: the same (query, referenceTime)
// always yields the same intent, which keeps the ADR-0103 D7 fingerprint promise.

// TemporalOrder is the order a query asks for.
type TemporalOrder string

const (
	OrderNone     TemporalOrder = "none"     // no order asked: recency off
	OrderLatest   TemporalOrder = "latest"   // "most recently", "latest": recency decay
	OrderEarliest TemporalOrder = "earliest" // "the first time", "originally": recency inverted
	OrderCurrent  TemporalOrder = "current"  // "currently", "now", "still": recency decay
	OrderHistory  TemporalOrder = "history"  // "how has X changed", "used to": recency off (every version matters)
)

// TimeBand is a target interval [From, To) with the softness of the phrase that
// named it. Sigma is the σ of the temporal-fit feature: a chunk one σ outside the
// band keeps exp(-1) ≈ 0.37 of the credit.
type TimeBand struct {
	From, To  time.Time
	Precision string // day | week | month | year | range
	Sigma     time.Duration
}

// TimeIntent is what a query says about time. The zero value (no target, order
// none) means "no time signal": Stage A then scores the query as if D3 were off.
type TimeIntent struct {
	Target   *TimeBand
	Order    TemporalOrder
	Explicit bool   // the target is an absolute range ("in March 2023"): pushed down as a hard window
	Phrase   string // the matched date phrase, for logs and tests
}

// RecencyDirection maps the order onto the Stage-A recency term: +1 = decay (newer
// is better), -1 = inverted (older is better), 0 = off.
func (i TimeIntent) RecencyDirection() int {
	switch i.Order {
	case OrderLatest, OrderCurrent:
		return 1
	case OrderEarliest:
		return -1
	default:
		return 0
	}
}

// Fit is the temporal-fit feature for a chunk whose event span is [start, end]:
// exp(-(Δ/σ)²), Δ = the gap between the span and the band (0 when they overlap).
// No target ⇒ 0 (a missing signal contributes nothing, ADR-0136 D1).
func (i TimeIntent) Fit(start, end time.Time) float64 {
	b := i.Target
	if b == nil || start.IsZero() || b.Sigma <= 0 {
		return 0
	}
	if end.Before(start) {
		end = start
	}
	var gap time.Duration
	switch {
	case end.Before(b.From):
		gap = b.From.Sub(end)
	case !start.Before(b.To):
		gap = start.Sub(b.To)
	}
	x := float64(gap) / float64(b.Sigma)
	return math.Exp(-x * x)
}

// unitSigma is the softness per unit: half a unit for week/month/year bands (a
// chunk one half-unit outside keeps 0.37), a full day for day bands, because
// session dates and "N days ago" both round to days.
var unitSigma = map[string]time.Duration{
	"day":   24 * time.Hour,
	"week":  84 * time.Hour,       // 3.5 d
	"month": 15 * 24 * time.Hour,  // half a month
	"year":  182 * 24 * time.Hour, // half a year
}

var (
	// dateParser and dateParserMu: go-dateparser v1.2.4 is NOT safe for concurrent
	// use, even across Parser values — its internal/strutil keeps package-level
	// golang.org/x/text transform.Chain transformers, which are stateful, and two
	// concurrent Search calls panic inside transform.String (slice bounds out of
	// range; found by TestTemporalIntent_ConcurrentUse). Every call is therefore
	// serialised. A Search costs ~120 µs, so the lock is negligible next to a recall's
	// embedding and SQL; recalls run concurrently, which is why it is needed at all.
	dateParser   dps.Parser
	dateParserMu sync.Mutex

	// quantifiers the library does not read as numbers. "a couple of days ago" is a
	// LongMemEval phrase (9a707b82) the library otherwise misses entirely.
	quantifierRe = regexp.MustCompile(`(?i)\b(a couple of|a couple|a few)\b`)
	quantifierTo = map[string]string{"a couple of": "2", "a couple": "2", "a few": "3"}

	yearRe = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	// rangeLeadRe: the match follows "last"/"past"/"since" — "in the last two weeks",
	// "since January" — so the band runs from the parsed time up to the asker's now.
	rangeLeadRe = regexp.MustCompile(`(?i)\b(last|past|since)\s*$`)

	// Order lexicon, lowercase, word-bounded. Precedence is resolved in classifyOrder.
	orderHistoryRe  = regexp.MustCompile(`\b(changed|change|changes|used to|previously|over time|history|evolved|how has|how have|over the (years|months|weeks))\b`)
	orderEarliestRe = regexp.MustCompile(`\b(first time|the first|(my|our|his|her|their) first|earliest|initially|originally|at first|in the beginning|oldest)\b`)
	orderCurrentRe  = regexp.MustCompile(`\b(currently|current|right now|now|at the moment|these days|nowadays|at present|still|anymore|any more)\b`)
	orderLatestRe   = regexp.MustCompile(`\b(most recent|most recently|latest|last time|recently|newest)\b`)

	// noiseMatches are strings the library returns as dates that are not time
	// expressions in a question: "how many days ago did I…" yields "ago"; "now" is
	// an ORDER word (current), not a target.
	noiseMatches = map[string]bool{"ago": true, "now": true, "in": true, "on": true, "at": true, "from now": true, "right now": true}
)

// TemporalIntent returns the time intent of query as seen from referenceTime (the
// asker's "now", ADR-0134 D4). A zero referenceTime means no "now" is known: a
// relative phrase then has no target (there is nothing to be relative to), while
// an absolute one ("in March 2023") still does. The order is read either way.
func TemporalIntent(query string, referenceTime time.Time) TimeIntent {
	out := TimeIntent{Order: classifyOrder(strings.ToLower(query))}
	ref := referenceTime.UTC()
	norm := quantifierRe.ReplaceAllStringFunc(query, func(m string) string {
		return quantifierTo[strings.ToLower(m)]
	})

	// The library fills the parts a phrase leaves out from CurrentTime. With no
	// reference time only explicit (year-bearing) phrases are kept, whose bands do
	// not depend on the fill-in, so any fixed anchor is correct — and fixed keeps
	// the function pure (it never reads the clock).
	anchor := ref
	if ref.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	cfg := &dps.Configuration{
		CurrentTime:         anchor,
		DefaultTimezone:     time.UTC,
		Languages:           []string{"en"},
		PreferredDateSource: dps.Past, // a memory question asks about the past: "in March" = the last March
	}
	found, err := searchDates(cfg, norm)
	if err != nil {
		return out // parser failure ⇒ no time signal; retrieval fails open
	}
	lowerNorm := strings.ToLower(norm)
	for _, m := range found {
		band, explicit, ok := bandOf(m.Text, m.Date, lowerNorm, ref)
		if !ok {
			continue
		}
		out.Target, out.Explicit, out.Phrase = band, explicit, m.Text
		break // the first time phrase is the question's anchor
	}
	return out
}

// searchDates runs the library under its lock. A panic inside the library becomes
// an error: retrieval reads fail open (AGENTS.md §3.2), so an input the parser
// chokes on costs that query its time signal, never the recall.
func searchDates(cfg *dps.Configuration, text string) (found []dps.SearchResult, err error) {
	dateParserMu.Lock()
	defer dateParserMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			found, err = nil, fmt.Errorf("go-dateparser panic: %v", r)
		}
	}()
	_, found, err = dateParser.Search(cfg, text)
	return found, err
}

// bandOf turns one library match into a target band. It rejects matches that are
// not time expressions, and relative phrases when no reference time is known.
func bandOf(text string, d date.Date, lowerQuery string, ref time.Time) (*TimeBand, bool, bool) {
	t := strings.TrimSpace(strings.Trim(text, " .,:;?!()[]'\""))
	lt := strings.ToLower(t)
	if t == "" || d.IsZero() || noiseMatches[lt] {
		return nil, false, false
	}
	// A single lowercase word ("may", "march", "sun") is a verb or a noun far more
	// often than a date in a question; a month or weekday NAME is capitalised.
	if !strings.Contains(t, " ") && t == lt {
		switch lt {
		case "yesterday", "today", "tomorrow":
		default:
			return nil, false, false
		}
	}
	explicit := yearRe.MatchString(t)
	if !explicit && ref.IsZero() {
		return nil, false, false // relative to an unknown "now": no target
	}
	at := d.Time.UTC()
	unit := unitOf(lt, d.Period)

	// "in the last two weeks" / "since January": the band runs from the parsed
	// point to the asker's now.
	if idx := strings.Index(lowerQuery, lt); idx >= 0 && rangeLeadRe.MatchString(lowerQuery[:idx]) && !strings.Contains(lt, "ago") {
		from := startOf(at, unit)
		to := ref
		if to.IsZero() || !from.Before(to) {
			to = endOf(from, unit)
		}
		return &TimeBand{From: from, To: to, Precision: "range", Sigma: unitSigma[unit]}, explicit, true
	}

	var from, to time.Time
	switch unit {
	case "week":
		// ADR-0136 D3: "two weeks ago" → about a 7-day band centred on ref − 14 d.
		from, to = at.Add(-unitSigma["week"]), at.Add(unitSigma["week"])
	default:
		from = startOf(at, unit)
		to = endOf(from, unit)
	}
	return &TimeBand{From: from, To: to, Precision: unit, Sigma: unitSigma[unit]}, explicit, true
}

// unitOf is the granularity a phrase names: the unit word when there is one
// ("two weeks ago" is a week phrase although the library reports day precision),
// else the library's own precision.
func unitOf(lowerText string, p date.Period) string {
	switch {
	case strings.Contains(lowerText, "week"):
		return "week"
	case strings.Contains(lowerText, "month"):
		return "month"
	case strings.Contains(lowerText, "year"):
		return "year"
	}
	switch p {
	case date.Month:
		return "month"
	case date.Year:
		return "year"
	default:
		return "day"
	}
}

func startOf(t time.Time, unit string) time.Time {
	switch unit {
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "year":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case "week":
		return t
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

func endOf(from time.Time, unit string) time.Time {
	switch unit {
	case "month":
		return from.AddDate(0, 1, 0)
	case "year":
		return from.AddDate(1, 0, 0)
	case "week":
		return from.AddDate(0, 0, 7)
	default:
		return from.AddDate(0, 0, 1)
	}
}

// classifyOrder resolves the order lexicon. A history cue wins (every version
// matters); a question that asks for both ends ("my first job and my current one")
// is a history question too.
func classifyOrder(lower string) TemporalOrder {
	hist := orderHistoryRe.MatchString(lower)
	early := orderEarliestRe.MatchString(lower)
	cur := orderCurrentRe.MatchString(lower)
	late := orderLatestRe.MatchString(lower)
	switch {
	case hist, early && (cur || late):
		return OrderHistory
	case early:
		return OrderEarliest
	case cur:
		return OrderCurrent
	case late:
		return OrderLatest
	default:
		return OrderNone
	}
}

// hardWindow is the event-time window an EXPLICIT target pushes down (ADR-0134 D5b
// via SearchOptions.Since/Until). A relative phrase never becomes a hard filter:
// "two weeks ago" is approximate, and a wrong hard window loses the evidence
// outright, where a soft feature only demotes it.
func (i TimeIntent) hardWindow() (domain.EventTimeQuery, bool) {
	if !i.Explicit || i.Target == nil {
		return domain.EventTimeQuery{}, false
	}
	return domain.EventTimeQuery{EventAfter: i.Target.From, EventBefore: i.Target.To}, true
}
