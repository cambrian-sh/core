package kernel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1866 (ADR-0137 D5.2): a replayed journal record carries the instant the
// journal accepted the signal, which the reactive engine stamps as the replay's
// ingest origin when the signal itself has no Timestamp.
func TestReactiveJournal_ReplayCarriesReceivedAt(t *testing.T) {
	j, done := openDeliveryJournal(t, filepath.Join(t.TempDir(), "agents.db"))
	defer done()
	before := time.Now().UTC()
	if _, err := j.AppendSignal(domain.Signal{StreamID: "s1", RawText: "x"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	recs, err := j.ReplayStreams(nil, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("replay: %d records, err %v", len(recs), err)
	}
	if got := recs[0].ReceivedAt; got.Before(before) || got.After(after) {
		t.Fatalf("ReceivedAt = %v, want the append time in [%v, %v]", got, before, after)
	}
}
