package domain

import (
	"fmt"
	"strings"
	"time"
)

// The current date, as the kernel tells it to every model that plans or does work
// (T-20261107-planner-and-chat-agent-are-not-told-today-s).
//
// Nothing in the kernel told a model what day it was. The planner, the chat front
// desk and the workers resolved "the last two weeks", "this quarter" and "tomorrow"
// against whatever date the model assumed — the provider's own system prompt where
// one happened to carry it, a guess everywhere else — and then did the date
// arithmetic themselves: o1-pipeline-review delegated "no activity since November 24
// (two weeks ago from today, November 7)" and two opportunities active inside the
// window were reported stale.
//
// Calendar is the kernel's ONE source of that date. It is built once at the
// composition root from the organisation's timezone (config `organisation.timezone`,
// else UTC) and shared by the planner, the replanner, the plan executor (which stamps
// it on every step: HandoffKeyCurrentDate) and the chat turn handoff. Its Block also
// carries the reference dates a relative ask needs (yesterday, this week, this month,
// this quarter, 7/14/30 days ago), computed here, so no model has to subtract days.
// The clock is injectable (NewCalendar's now) so every consumer is testable on a
// fixed date; nothing else in the kernel reads the wall clock for a prompt's "today".

// HandoffKeyCurrentDate carries Calendar.Block on a plan step's handoff context.
// Kernel-authored: the executor always overwrites it on dispatch, so a caller's
// metadata cannot assert another date, and it is stripped from step responses so it
// never merges into the plan context as an agent-written key.
const HandoffKeyCurrentDate = "_current_date"

// Calendar answers "what is today" for prompts. The zero value and a nil *Calendar
// are usable: the wall clock in UTC.
type Calendar struct {
	now func() time.Time
	loc *time.Location
}

// NewCalendar returns a Calendar in loc (nil ⇒ UTC) on the clock now (nil ⇒ time.Now).
func NewCalendar(loc *time.Location, now func() time.Time) *Calendar {
	return &Calendar{now: now, loc: loc}
}

// Location is the timezone the calendar's dates are in.
func (c *Calendar) Location() *time.Location {
	if c == nil || c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// Now is the calendar's current instant, in its Location.
func (c *Calendar) Now() time.Time {
	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	return now().In(c.Location())
}

// Date is today's ISO date ("2026-11-07") in the calendar's Location.
func (c *Calendar) Date() string { return c.Now().Format(time.DateOnly) }

// Block is the current-date block the kernel shows a model: today's ISO date and
// weekday, the timezone, and the reference dates of the common relative asks.
func (c *Calendar) Block() string { return CalendarBlock(c.Now()) }

// CalendarBlock renders the block for the instant t, in t's location. Pure, so a
// test pins it with any time.
//
//	today: 2026-11-07 (Saturday)
//	timezone: UTC
//	yesterday: 2026-11-06 (Friday)
//	tomorrow: 2026-11-08 (Sunday)
//	this week: 2026-11-02 (Monday) to 2026-11-08 (Sunday)
//	this month: 2026-11-01 to 2026-11-30
//	this quarter: Q4 2026, 2026-10-01 to 2026-12-31
//	7 days ago: 2026-10-31 (Saturday)
//	14 days ago: 2026-10-24 (Saturday)
//	30 days ago: 2026-10-08 (Thursday)
func CalendarBlock(t time.Time) string {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	withDay := func(x time.Time) string { return x.Format(time.DateOnly) + " (" + x.Weekday().String() + ")" }
	iso := func(x time.Time) string { return x.Format(time.DateOnly) }

	// ISO weeks start on Monday.
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	monthStart := time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
	q := (int(m)-1)/3 + 1
	quarterStart := time.Date(y, time.Month(3*(q-1)+1), 1, 0, 0, 0, 0, t.Location())

	tz := t.Location().String()
	if tz == "" || tz == "Local" {
		tz = t.Format("MST -07:00")
	}
	lines := []string{
		"today: " + withDay(day),
		"timezone: " + tz,
		"yesterday: " + withDay(day.AddDate(0, 0, -1)),
		"tomorrow: " + withDay(day.AddDate(0, 0, 1)),
		"this week: " + withDay(monday) + " to " + withDay(monday.AddDate(0, 0, 6)),
		"this month: " + iso(monthStart) + " to " + iso(monthStart.AddDate(0, 1, -1)),
		fmt.Sprintf("this quarter: Q%d %d, %s to %s", q, y, iso(quarterStart), iso(quarterStart.AddDate(0, 3, -1))),
		"7 days ago: " + withDay(day.AddDate(0, 0, -7)),
		"14 days ago: " + withDay(day.AddDate(0, 0, -14)),
		"30 days ago: " + withDay(day.AddDate(0, 0, -30)),
	}
	return strings.Join(lines, "\n")
}

// StampCurrentDate writes block onto a step's handoff context under
// HandoffKeyCurrentDate, or removes the key when block is empty, so a stale or
// caller-supplied value never survives. ctx must be non-nil.
func StampCurrentDate(ctx map[string]string, block string) {
	if strings.TrimSpace(block) == "" {
		delete(ctx, HandoffKeyCurrentDate)
		return
	}
	ctx[HandoffKeyCurrentDate] = block
}
