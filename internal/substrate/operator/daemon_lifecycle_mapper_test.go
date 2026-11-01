package operator

import (
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0070 D2: a quarantined daemon is operator-visible. It reaches the console
// as an AuditOp (no contract change; see the mapper), with an id derived from the
// feed seq so a replay is idempotent in the console's audit tail.
func TestToOperatorEvent_DaemonQuarantined(t *testing.T) {
	t.Parallel()
	se := domain.SequencedEvent{
		Seq: 77,
		At:  time.Unix(1750000000, 0).UTC(),
		Event: domain.DaemonQuarantinedEvent{
			AgentID: "watcher-a", StreamID: "s-1", Reason: "crash loop", Attempts: 5,
		},
	}
	a := toOperatorEvent(se).GetAudit()
	if a == nil {
		t.Fatalf("expected AuditOp payload, got %T", toOperatorEvent(se).GetPayload())
	}
	if a.GetId() != "daemon.quarantined:77" {
		t.Errorf("id = %q, want seq-derived daemon.quarantined:77", a.GetId())
	}
	if a.GetActionType() != domain.EventTypeDaemonQuarantined || a.GetTargetType() != "agent" ||
		a.GetTargetId() != "watcher-a" || a.GetResult() != "quarantined" {
		t.Errorf("unexpected mapping: %+v", a)
	}
	for _, want := range []string{"crash loop", "5 restart attempts", "stream s-1"} {
		if !strings.Contains(a.GetReason(), want) {
			t.Errorf("reason %q missing %q", a.GetReason(), want)
		}
	}
	if toOperatorEvent(se).GetAudit().GetId() != a.GetId() {
		t.Error("mapping is not deterministic for the same seq")
	}
}

func TestToOperatorEvent_DaemonRecovered(t *testing.T) {
	t.Parallel()
	se := domain.SequencedEvent{Seq: 78, Event: domain.DaemonRecoveredEvent{AgentID: "watcher-a", StreamID: "s-1"}}
	a := toOperatorEvent(se).GetAudit()
	if a == nil || a.GetId() != "daemon.recovered:78" || a.GetResult() != "recovered" || a.GetTargetId() != "watcher-a" {
		t.Fatalf("unexpected mapping: %+v", a)
	}
}

// The ROUTE-08.A lesson, stated for these two directly: a mapper case without a
// bridge entry is built, published and never delivered.
func TestFeedBridge_DaemonLifecycleEventsAreBridged(t *testing.T) {
	t.Parallel()
	want := map[string]bool{domain.EventTypeDaemonQuarantined: false, domain.EventTypeDaemonRecovered: false}
	for _, tp := range feedEventTypes {
		if _, ok := want[tp]; ok {
			want[tp] = true
		}
	}
	for tp, ok := range want {
		if !ok {
			t.Errorf("%s has a mapper case but is not in feedEventTypes", tp)
		}
	}
}
