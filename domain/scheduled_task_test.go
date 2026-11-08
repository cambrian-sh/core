package domain_test

import (
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestRRuleToCron(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"FREQ=WEEKLY;BYDAY=MO;BYHOUR=8;BYMINUTE=0": "0 8 * * 1",
		"RRULE:FREQ=WEEKLY;BYDAY=FR,MO;BYHOUR=17":  "0 17 * * 1,5",
		"FREQ=DAILY;BYHOUR=7;BYMINUTE=30":          "30 7 * * *",
		"FREQ=DAILY;BYHOUR=9;BYDAY=MO,TU,WE,TH,FR": "0 9 * * 1,2,3,4,5",
		"FREQ=HOURLY;BYMINUTE=15":                  "15 * * * *",
		"FREQ=HOURLY;INTERVAL=4":                   "0 */4 * * *",
		"FREQ=MONTHLY;BYMONTHDAY=1;BYHOUR=6":       "0 6 1 * *",
		"freq=weekly;byday=su;byhour=20;wkst=mo":   "0 20 * * 0",
	}
	for in, want := range ok {
		got, err := domain.RRuleToCron(in)
		if err != nil || got != want {
			t.Errorf("RRuleToCron(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "FREQ=YEARLY", "FREQ=DAILY;COUNT=5", "FREQ=DAILY;UNTIL=20270101T000000Z",
		"FREQ=WEEKLY", "FREQ=WEEKLY;BYDAY=1MO", "FREQ=DAILY;INTERVAL=2", "FREQ=MONTHLY",
		"FREQ=DAILY;BYHOUR=25", "BYHOUR=8", "FREQ=HOURLY;BYHOUR=3",
	}
	for _, in := range bad {
		if got, err := domain.RRuleToCron(in); err == nil || !errors.Is(err, domain.ErrScheduleInvalid) {
			t.Errorf("RRuleToCron(%q) = %q, %v; want ErrScheduleInvalid", in, got, err)
		}
	}
}

func TestScheduledTaskID_Deterministic(t *testing.T) {
	t.Parallel()
	a := domain.ScheduledTaskSpec{OwnerID: "lena", Cron: "0 8 * * 1", Goal: "summary", Delivery: "conversation"}
	b := a
	b.Title, b.ApprovedBy, b.ProposedBy = "different title", "someone", "agent"
	if domain.ScheduledTaskID(a) != domain.ScheduledTaskID(b) {
		t.Fatal("title/approver/proposer must not change the identity")
	}
	c := a
	c.OwnerID = "dana"
	if domain.ScheduledTaskID(a) == domain.ScheduledTaskID(c) {
		t.Fatal("a different owner is a different task")
	}
}
