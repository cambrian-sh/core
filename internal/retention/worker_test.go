package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type fakeTable struct {
	mu      sync.Mutex
	rows    []time.Time
	gotLim  int
	gotFrom time.Time
	err     error
}

func (f *fakeTable) purge(_ context.Context, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.gotLim, f.gotFrom = limit, before
	var keep []time.Time
	var n int64
	for _, r := range f.rows {
		if r.Before(before) && int(n) < limit {
			n++
			continue
		}
		keep = append(keep, r)
	}
	f.rows = keep
	return n, nil
}

func TestRunOnce_PrunesOnlyPastRetentionAndDrainsInBatches(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	audit := &fakeTable{}
	for i := 0; i < 7; i++ {
		audit.rows = append(audit.rows, now.Add(-400*24*time.Hour)) // past 365 d
	}
	audit.rows = append(audit.rows, now.Add(-10*24*time.Hour)) // recent
	off := &fakeTable{rows: []time.Time{now.Add(-1000 * 24 * time.Hour)}}

	w := New([]Rule{
		{Name: "operator_audit", Keep: 365 * 24 * time.Hour, Purge: audit.purge},
		{Name: "chunk_triplets", Keep: 0, Purge: off.purge}, // default off
	}, time.Hour, 3, nil)
	w.now = func() time.Time { return now }

	if got := w.Active(); len(got) != 1 || got[0] != "operator_audit" {
		t.Fatalf("active rules = %v; a 0-day rule must not run", got)
	}
	removed := w.RunOnce(context.Background())
	if removed["operator_audit"] != 7 {
		t.Fatalf("removed %d, want all 7 old rows over several batches", removed["operator_audit"])
	}
	if len(audit.rows) != 1 {
		t.Errorf("the recent row must survive; left %v", audit.rows)
	}
	if audit.gotLim != 3 || !audit.gotFrom.Equal(now.Add(-365*24*time.Hour)) {
		t.Errorf("purge called with limit %d before %v", audit.gotLim, audit.gotFrom)
	}
	if len(off.rows) != 1 {
		t.Error("a disabled rule must not delete anything")
	}
}

func TestRunOnce_OneFailingRuleDoesNotStopOthers(t *testing.T) {
	bad := &fakeTable{err: errors.New("db down")}
	good := &fakeTable{rows: []time.Time{time.Now().Add(-48 * time.Hour)}}
	w := New([]Rule{
		{Name: "bad", Keep: time.Hour, Purge: bad.purge},
		{Name: "good", Keep: time.Hour, Purge: good.purge},
	}, time.Hour, 10, nil)
	if got := w.RunOnce(context.Background()); got["good"] != 1 {
		t.Fatalf("removed = %v", got)
	}
}

func TestStartStop_NoRulesNoGoroutineAndCleanStop(t *testing.T) {
	New(nil, time.Millisecond, 10, nil).Start(context.Background()) // no-op, goleak checks

	tbl := &fakeTable{rows: []time.Time{time.Now().Add(-48 * time.Hour)}}
	w := New([]Rule{{Name: "t", Keep: time.Hour, Purge: tbl.purge}}, time.Millisecond, 10, nil)
	w.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		tbl.mu.Lock()
		n := len(tbl.rows)
		tbl.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	w.Stop()
	w.Stop()
	if len(tbl.rows) != 0 {
		t.Error("the running worker never pruned")
	}
}
