package memory

import (
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0134 D5a — chronological context.
//
// With execution.retrieval.chronological_context_enabled the passages handed to the
// FINAL answer synthesis are ordered by event time and each carries its date, so the
// reader sees what happened when ("I booked Lisbon" before "the trip was cancelled")
// instead of a relevance-ordered jumble with no dates. Retrieval is untouched: the
// same passages are SELECTED by relevance (the first n rows); only their order and
// dating in the reader's context change. Off by default — reordering the context of
// an evidence-bound reader can hurt it (failure archaeology E7), so it is turned on
// only with a recorded A/B (ADR-0134 D5).

// EnableChronologicalContext turns D5a on (wired from config in app.go).
func (q *QueryService) EnableChronologicalContext() { q.chronoContext = true }

// synthesisContext returns the rows the reader will cite, in the order it sees them,
// and the passage texts for the synthesizer: the first n rows (the relevance
// selection). Off: unchanged order, bare texts — the behaviour before D5a. On: those n
// rows ordered by event time and prefixed with their date; rows whose time is only
// the ingest clock (ingest_default: the content's own date is unknown) follow the
// dated ones in their relevance order, labelled as such, because interleaving a
// recording date with real event dates would misstate when they happened.
//
// rows beyond n are returned after the context rows, untouched, so a caller whose
// citations index into the returned slice ([n] -> rows[n-1], ADR-0081) stays aligned.
func (q *QueryService) synthesisContext(rows []domain.SearchResult, n int) ([]domain.SearchResult, []string) {
	if n > len(rows) {
		n = len(rows)
	}
	if !q.chronoContext {
		texts := make([]string, 0, n)
		for _, r := range rows[:n] {
			texts = append(texts, r.Document.Text)
		}
		return rows, texts
	}
	head := append([]domain.SearchResult(nil), rows[:n]...)
	sort.SliceStable(head, func(i, j int) bool {
		hi, hj := domain.HitEventTime(head[i].Document), domain.HitEventTime(head[j].Document)
		di, dj := hi.Source != domain.TimeSourceIngestDefault, hj.Source != domain.TimeSourceIngestDefault
		if di != dj {
			return di // dated before undated
		}
		if !di {
			return false // both undated: keep relevance order
		}
		return hi.EventTime.Before(hj.EventTime)
	})
	texts := make([]string, 0, n)
	for _, r := range head {
		texts = append(texts, "[Date: "+eventDateLabel(domain.HitEventTime(r.Document))+"]\n"+r.Document.Text)
	}
	out := make([]domain.SearchResult, 0, len(rows))
	out = append(out, head...)
	out = append(out, rows[n:]...)
	return out, texts
}

// eventDateLabel renders a hit's event time at its recorded precision (a date-only
// source is never shown as an exact instant, owner decision Q3), with its span and an
// honest qualifier for anything that is not a stated time.
func eventDateLabel(h domain.HitTime) string {
	format := func(t time.Time) string {
		t = t.UTC()
		switch h.Precision {
		case domain.TimePrecisionYear:
			return t.Format("2006")
		case domain.TimePrecisionMonth:
			return t.Format("2006-01")
		case domain.TimePrecisionDay:
			return t.Format("2006-01-02")
		case domain.TimePrecisionHour:
			return t.Format("2006-01-02 15:00 UTC")
		case domain.TimePrecisionMinute:
			return t.Format("2006-01-02 15:04 UTC")
		default:
			return t.Format("2006-01-02 15:04:05 UTC")
		}
	}
	label := format(h.EventTime)
	if end := h.End(); end.After(h.EventTime) && format(end) != label {
		label += " to " + format(end)
	}
	switch h.Source {
	case domain.TimeSourceIngestDefault:
		return "unknown (recorded " + label + ")"
	case domain.TimeSourceInferred:
		return label + " (inferred)"
	case domain.TimeSourceClampedFuture:
		return label + " (stated time was in the future; clamped to when it was recorded)"
	}
	return label
}
