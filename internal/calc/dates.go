package calc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// WorkCalendar is the organisation's working-day calendar: its weekend days and
// its holidays (organisation.weekend / organisation.holidays). The zero value has
// a Saturday-Sunday weekend and no holidays.
type WorkCalendar struct {
	weekend   map[time.Weekday]bool
	dated     map[string]bool // YYYY-MM-DD
	recurring map[string]bool // MM-DD
}

// NewWorkCalendar builds a calendar. weekend nil ⇒ Saturday and Sunday. A holiday
// is "YYYY-MM-DD" (that day) or "MM-DD" (that day every year); anything else is an
// error (config validation refuses it first).
func NewWorkCalendar(weekend []time.Weekday, holidays []string) (WorkCalendar, error) {
	w := WorkCalendar{weekend: map[time.Weekday]bool{}, dated: map[string]bool{}, recurring: map[string]bool{}}
	if weekend == nil {
		weekend = []time.Weekday{time.Saturday, time.Sunday}
	}
	for _, d := range weekend {
		w.weekend[d] = true
	}
	for _, h := range holidays {
		h = strings.TrimSpace(h)
		if _, err := time.Parse("2006-01-02", h); err == nil {
			w.dated[h] = true
			continue
		}
		if _, err := time.Parse("01-02", h); err == nil {
			w.recurring[h] = true
			continue
		}
		return WorkCalendar{}, fmt.Errorf("holiday %q is neither YYYY-MM-DD nor MM-DD", h)
	}
	return w, nil
}

func (w WorkCalendar) isWeekend(t time.Time) bool {
	if w.weekend == nil {
		return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
	}
	return w.weekend[t.Weekday()]
}

// IsHoliday reports whether t is one of the organisation's holidays.
func (w WorkCalendar) IsHoliday(t time.Time) bool {
	return w.dated[iso(t)] || w.recurring[t.Format("01-02")]
}

// IsWorkingDay reports whether t is neither a weekend day nor a holiday.
func (w WorkCalendar) IsWorkingDay(t time.Time) bool { return !w.isWeekend(t) && !w.IsHoliday(t) }

// WorkingDays counts the working days from start to end, both included, with the
// holidays that fell on would-be working days. end before start counts from end
// to start and returns a negative count.
func (w WorkCalendar) WorkingDays(start, end time.Time) (n int, holidays []string) {
	sign := 1
	if end.Before(start) {
		start, end, sign = end, start, -1
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		switch {
		case w.IsWorkingDay(d):
			n++
		case !w.isWeekend(d):
			holidays = append(holidays, iso(d))
		}
	}
	return sign * n, holidays
}

// AddWorkingDays moves n working days from start (negative ⇒ backwards); the
// start day itself is not counted. n = 0 returns start.
func (w WorkCalendar) AddWorkingDays(start time.Time, n int) time.Time {
	step := 1
	if n < 0 {
		step, n = -1, -n
	}
	d := start
	for n > 0 {
		d = d.AddDate(0, 0, step)
		if w.IsWorkingDay(d) {
			n--
		}
	}
	return d
}

// ─── calc_date_window ────────────────────────────────────────────────────────

type dateWindowTool struct{ c *core }

// windows lists the calc_date_window names, in the order the description shows.
var windows = []string{"today", "yesterday", "tomorrow", "this_week", "last_week", "next_week",
	"this_month", "last_month", "next_month", "this_quarter", "last_quarter", "next_quarter",
	"this_year", "last_year", "last_n_days", "next_n_days", "n_days_ago", "n_days_ahead"}

func (dateWindowTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolDateWindow,
		Description: "Resolve a relative date or period (\"last week\", \"this quarter\", \"the last 14 days\") to exact dates " +
			"from the organisation's calendar. Never compute dates yourself. window: " + strings.Join(windows, ", ") +
			". last_n_days is anchor minus n through anchor; next_n_days is anchor through anchor plus n; n_days_ago and " +
			"n_days_ahead are single days. Weeks start on Monday. anchor defaults to today. Returns start, end, the " +
			"calendar and working days in it, and ledger_ref (cite it for the dates).",
		Schema: []byte(`{"type":"object","properties":{` +
			`"window":{"type":"string","enum":["` + strings.Join(windows, `","`) + `"]},` +
			`"n":{"type":"integer","minimum":0,"description":"days, for the *_n_days windows"},` +
			`"anchor":{"type":"string","description":"YYYY-MM-DD; default today"}},` +
			`"required":["window"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

func (t dateWindowTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a struct {
		Window string `json:"window"`
		N      *int   `json:"n"`
		Anchor string `json:"anchor"`
	}
	if err := decodeArgs(ToolDateWindow, call.ArgsJSON, &a); err != nil {
		return nil, err
	}
	anchor := t.c.today()
	if strings.TrimSpace(a.Anchor) != "" {
		d, ok := ParseDate(a.Anchor)
		if !ok {
			return nil, fmt.Errorf("%s: anchor %q is not a date (YYYY-MM-DD)", ToolDateWindow, a.Anchor)
		}
		anchor = d
	}
	start, end, err := window(strings.ToLower(strings.TrimSpace(a.Window)), a.N, anchor)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ToolDateWindow, err)
	}
	wd, hol := t.c.d.Work.WorkingDays(start, end)
	res := map[string]any{
		"window": a.Window, "anchor": iso(anchor),
		"start": iso(start), "start_weekday": start.Weekday().String(),
		"end": iso(end), "end_weekday": end.Weekday().String(),
		"calendar_days": int(end.Sub(start).Hours()/24) + 1, "working_days": wd,
	}
	if len(hol) > 0 {
		res["holidays"] = hol
	}
	return t.c.finish(ctx, call, ToolDateWindow, nil, res)
}

// window resolves a named window around anchor (both ends included).
func window(name string, n *int, anchor time.Time) (time.Time, time.Time, error) {
	needN := func() (int, error) {
		if n == nil {
			return 0, fmt.Errorf("window %s needs n", name)
		}
		return *n, nil
	}
	y, m, _ := anchor.Date()
	monday := anchor.AddDate(0, 0, -((int(anchor.Weekday()) + 6) % 7))
	monthStart := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	qStart := time.Date(y, time.Month(3*((int(m)-1)/3)+1), 1, 0, 0, 0, 0, time.UTC)
	yearStart := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	switch name {
	case "today":
		return anchor, anchor, nil
	case "yesterday":
		d := anchor.AddDate(0, 0, -1)
		return d, d, nil
	case "tomorrow":
		d := anchor.AddDate(0, 0, 1)
		return d, d, nil
	case "this_week":
		return monday, monday.AddDate(0, 0, 6), nil
	case "last_week":
		return monday.AddDate(0, 0, -7), monday.AddDate(0, 0, -1), nil
	case "next_week":
		return monday.AddDate(0, 0, 7), monday.AddDate(0, 0, 13), nil
	case "this_month":
		return monthStart, monthStart.AddDate(0, 1, -1), nil
	case "last_month":
		s := monthStart.AddDate(0, -1, 0)
		return s, monthStart.AddDate(0, 0, -1), nil
	case "next_month":
		s := monthStart.AddDate(0, 1, 0)
		return s, s.AddDate(0, 1, -1), nil
	case "this_quarter":
		return qStart, qStart.AddDate(0, 3, -1), nil
	case "last_quarter":
		s := qStart.AddDate(0, -3, 0)
		return s, qStart.AddDate(0, 0, -1), nil
	case "next_quarter":
		s := qStart.AddDate(0, 3, 0)
		return s, s.AddDate(0, 3, -1), nil
	case "this_year":
		return yearStart, yearStart.AddDate(1, 0, -1), nil
	case "last_year":
		s := yearStart.AddDate(-1, 0, 0)
		return s, yearStart.AddDate(0, 0, -1), nil
	case "last_n_days":
		k, err := needN()
		return anchor.AddDate(0, 0, -k), anchor, err
	case "next_n_days":
		k, err := needN()
		return anchor, anchor.AddDate(0, 0, k), err
	case "n_days_ago":
		k, err := needN()
		d := anchor.AddDate(0, 0, -k)
		return d, d, err
	case "n_days_ahead":
		k, err := needN()
		d := anchor.AddDate(0, 0, k)
		return d, d, err
	}
	return time.Time{}, time.Time{}, fmt.Errorf("unknown window %q (one of %s)", name, strings.Join(windows, ", "))
}

// ─── calc_date_add ───────────────────────────────────────────────────────────

type dateAddTool struct{ c *core }

func (dateAddTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolDateAdd,
		Description: "Add or subtract time from a date: calendar days, weeks, months and years, or working days " +
			"(skipping the organisation's weekend and holidays; the start day is not counted). Negative values go " +
			"back. Use it for due dates, deadlines and \"N working days after\". date defaults to today. Returns the " +
			"date, its weekday and ledger_ref (cite it for the date).",
		Schema: []byte(`{"type":"object","properties":{` +
			`"date":{"type":"string","description":"YYYY-MM-DD; default today"},` +
			`"days":{"type":"integer"},"weeks":{"type":"integer"},"months":{"type":"integer"},"years":{"type":"integer"},` +
			`"working_days":{"type":"integer","description":"working days; not combined with the other units"}},` +
			`"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

func (t dateAddTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a struct {
		Date        string `json:"date"`
		Days        int    `json:"days"`
		Weeks       int    `json:"weeks"`
		Months      int    `json:"months"`
		Years       int    `json:"years"`
		WorkingDays *int   `json:"working_days"`
	}
	if err := decodeArgs(ToolDateAdd, call.ArgsJSON, &a); err != nil {
		return nil, err
	}
	from := t.c.today()
	if strings.TrimSpace(a.Date) != "" {
		d, ok := ParseDate(a.Date)
		if !ok {
			return nil, fmt.Errorf("%s: date %q is not a date (YYYY-MM-DD)", ToolDateAdd, a.Date)
		}
		from = d
	}
	var to time.Time
	res := map[string]any{"from": iso(from), "from_weekday": from.Weekday().String()}
	if a.WorkingDays != nil {
		if a.Days != 0 || a.Weeks != 0 || a.Months != 0 || a.Years != 0 {
			return nil, fmt.Errorf("%s: working_days cannot be combined with days, weeks, months or years", ToolDateAdd)
		}
		to = t.c.d.Work.AddWorkingDays(from, *a.WorkingDays)
		res["working_days"] = *a.WorkingDays
		if _, hol := t.c.d.Work.WorkingDays(from, to); len(hol) > 0 {
			res["holidays_skipped"] = hol
		}
	} else {
		to = from.AddDate(a.Years, a.Months, a.Days+7*a.Weeks)
		res["added"] = map[string]int{"years": a.Years, "months": a.Months, "weeks": a.Weeks, "days": a.Days}
		res["is_working_day"] = t.c.d.Work.IsWorkingDay(to)
	}
	res["date"] = iso(to)
	res["weekday"] = to.Weekday().String()
	return t.c.finish(ctx, call, ToolDateAdd, nil, res)
}

// ─── calc_working_days ───────────────────────────────────────────────────────

type workingDaysTool struct{ c *core }

func (workingDaysTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolWorkingDays,
		Description: "Count the working days between two dates with the organisation's weekend and holiday calendar " +
			"(both ends included unless include_start / include_end is false), plus the calendar days between them. " +
			"Use it for \"how many working days\", SLA and deadline checks. Returns the counts, the holidays in the " +
			"range and ledger_ref (cite it for the numbers).",
		Schema: []byte(`{"type":"object","properties":{` +
			`"start":{"type":"string","description":"YYYY-MM-DD"},"end":{"type":"string","description":"YYYY-MM-DD; default today"},` +
			`"include_start":{"type":"boolean"},"include_end":{"type":"boolean"}},` +
			`"required":["start"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

func (t workingDaysTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a struct {
		Start        string `json:"start"`
		End          string `json:"end"`
		IncludeStart *bool  `json:"include_start"`
		IncludeEnd   *bool  `json:"include_end"`
	}
	if err := decodeArgs(ToolWorkingDays, call.ArgsJSON, &a); err != nil {
		return nil, err
	}
	start, ok := ParseDate(a.Start)
	if !ok {
		return nil, fmt.Errorf("%s: start %q is not a date (YYYY-MM-DD)", ToolWorkingDays, a.Start)
	}
	end := t.c.today()
	if strings.TrimSpace(a.End) != "" {
		if end, ok = ParseDate(a.End); !ok {
			return nil, fmt.Errorf("%s: end %q is not a date (YYYY-MM-DD)", ToolWorkingDays, a.End)
		}
	}
	lo, hi, sign := start, end, 1
	if hi.Before(lo) {
		lo, hi, sign = hi, lo, -1
	}
	from, to := lo, hi
	// include_* name the request's own ends (start / end), whichever is earlier.
	inclStart, inclEnd := a.IncludeStart == nil || *a.IncludeStart, a.IncludeEnd == nil || *a.IncludeEnd
	if sign < 0 {
		inclStart, inclEnd = inclEnd, inclStart
	}
	if !inclStart {
		from = from.AddDate(0, 0, 1)
	}
	if !inclEnd {
		to = to.AddDate(0, 0, -1)
	}
	n, hol := 0, []string(nil)
	if !to.Before(from) {
		n, hol = t.c.d.Work.WorkingDays(from, to)
	}
	res := map[string]any{
		"start": iso(start), "end": iso(end),
		"working_days":  sign * n,
		"calendar_days": sign * int(hi.Sub(lo).Hours()/24),
		"include_start": a.IncludeStart == nil || *a.IncludeStart,
		"include_end":   a.IncludeEnd == nil || *a.IncludeEnd,
	}
	if len(hol) > 0 {
		sort.Strings(hol)
		res["holidays"] = hol
	}
	return t.c.finish(ctx, call, ToolWorkingDays, nil, res)
}

// ResultSource implements domain.KernelToolSource (ENG-531): the date tools read
// no content — their answers are computed from the arguments and the kernel's
// own calendar — so a call adds nothing to what the session consumed.
func (dateWindowTool) ResultSource() domain.SourceClass { return "" }

// ResultSource: see dateWindowTool.ResultSource.
func (dateAddTool) ResultSource() domain.SourceClass { return "" }

// ResultSource: see dateWindowTool.ResultSource.
func (workingDaysTool) ResultSource() domain.SourceClass { return "" }
