package domain

import (
	"strings"
	"testing"
	"time"
)

// The o1 business date: 2026-11-07, a Saturday. "The last two weeks" starts
// 2026-10-24 — the chat agent wrote "since November 24" (forward, not back).
func TestCalendarBlock_O1BusinessDate(t *testing.T) {
	t.Parallel()
	got := CalendarBlock(time.Date(2026, 11, 7, 18, 26, 0, 0, time.UTC))
	want := strings.Join([]string{
		"today: 2026-11-07 (Saturday)",
		"timezone: UTC",
		"yesterday: 2026-11-06 (Friday)",
		"tomorrow: 2026-11-08 (Sunday)",
		"this week: 2026-11-02 (Monday) to 2026-11-08 (Sunday)",
		"this month: 2026-11-01 to 2026-11-30",
		"this quarter: Q4 2026, 2026-10-01 to 2026-12-31",
		"7 days ago: 2026-10-31 (Saturday)",
		"14 days ago: 2026-10-24 (Saturday)",
		"30 days ago: 2026-10-08 (Thursday)",
	}, "\n")
	if got != want {
		t.Fatalf("block:\n%s\nwant:\n%s", got, want)
	}
}

// The date is the organisation's: 22:30 UTC on the 7th is already the 8th in UTC+3.
func TestCalendar_UsesItsLocationAndClock(t *testing.T) {
	t.Parallel()
	istanbul := time.FixedZone("Europe/Istanbul", 3*3600)
	now := func() time.Time { return time.Date(2026, 11, 7, 22, 30, 0, 0, time.UTC) }
	c := NewCalendar(istanbul, now)
	if got := c.Date(); got != "2026-11-08" {
		t.Errorf("Date = %s, want 2026-11-08", got)
	}
	b := c.Block()
	for _, line := range []string{"today: 2026-11-08 (Sunday)", "timezone: Europe/Istanbul",
		"this week: 2026-11-02 (Monday) to 2026-11-08 (Sunday)"} {
		if !strings.Contains(b, line) {
			t.Errorf("block lacks %q:\n%s", line, b)
		}
	}
	if utc := NewCalendar(nil, now).Date(); utc != "2026-11-07" {
		t.Errorf("nil location reads as UTC: %s", utc)
	}
}

func TestCalendarBlock_Boundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		at    time.Time
		lines []string
	}{
		{time.Date(2028, 3, 1, 9, 0, 0, 0, time.UTC), []string{ // leap year, Wednesday
			"yesterday: 2028-02-29 (Tuesday)", "this month: 2028-03-01 to 2028-03-31",
			"this quarter: Q1 2028, 2028-01-01 to 2028-03-31", "this week: 2028-02-28 (Monday) to 2028-03-05 (Sunday)"}},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), []string{
			"tomorrow: 2027-01-01 (Friday)", "this quarter: Q4 2026, 2026-10-01 to 2026-12-31"}},
		{time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), []string{ // a Monday
			"this week: 2026-11-02 (Monday) to 2026-11-08 (Sunday)"}},
	} {
		b := CalendarBlock(tt.at)
		for _, l := range tt.lines {
			if !strings.Contains(b, l) {
				t.Errorf("%s: block lacks %q:\n%s", tt.at, l, b)
			}
		}
	}
}

func TestCalendar_NilIsTheWallClockInUTC(t *testing.T) {
	t.Parallel()
	var c *Calendar
	if c.Location() != time.UTC {
		t.Error("nil calendar location must be UTC")
	}
	if got, want := c.Date(), time.Now().UTC().Format(time.DateOnly); got != want {
		// Could straddle midnight; recheck once.
		if got2 := c.Date(); got2 != time.Now().UTC().Format(time.DateOnly) {
			t.Errorf("nil calendar Date = %s, want %s", got, want)
		}
	}
}

func TestStampCurrentDate(t *testing.T) {
	t.Parallel()
	ctx := map[string]string{HandoffKeyCurrentDate: "forged"}
	StampCurrentDate(ctx, "today: 2026-11-07 (Saturday)")
	if ctx[HandoffKeyCurrentDate] != "today: 2026-11-07 (Saturday)" {
		t.Errorf("stamp did not overwrite: %q", ctx[HandoffKeyCurrentDate])
	}
	StampCurrentDate(ctx, "  ")
	if _, ok := ctx[HandoffKeyCurrentDate]; ok {
		t.Error("an empty block must remove the key")
	}
}
