package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261107-planner-and-chat-agent-are-not-told-today-s: every turn carries the
// kernel's date — today in the organisation's timezone, with the reference dates of
// the common relative asks — as "current_date", from the wired calendar.
func TestTurn_CarriesTheCurrentDateFromTheCalendar(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	istanbul := time.FixedZone("Europe/Istanbul", 3*3600)
	svc.SetCalendar(domain.NewCalendar(istanbul, func() time.Time { return time.Date(2026, 11, 7, 18, 26, 0, 0, time.UTC) }))
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1",
		Text: "Which of my opportunities closing this quarter haven't had any activity in the last two weeks?"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	hs := pool.all()
	if len(hs) != 1 {
		t.Fatalf("dispatches = %d", len(hs))
	}
	got := hs[0].Payload.Metadata["current_date"]
	for _, line := range []string{"today: 2026-11-07 (Saturday)", "timezone: Europe/Istanbul",
		"this quarter: Q4 2026, 2026-10-01 to 2026-12-31", "14 days ago: 2026-10-24 (Saturday)"} {
		if !strings.Contains(got, line) {
			t.Errorf("current_date lacks %q:\n%s", line, got)
		}
	}
}

// Without a wired calendar the turn is still dated: the wall clock in UTC.
func TestTurn_CurrentDateDefaultsToUTC(t *testing.T) {
	svc, _, pool := setup(t, domain.ProfileEmployee)
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	got := pool.all()[0].Payload.Metadata["current_date"]
	if !strings.Contains(got, "timezone: UTC") || !strings.HasPrefix(got, "today: ") {
		t.Errorf("current_date = %q", got)
	}
}
