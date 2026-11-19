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

// ENG-1292: the decorator's batched append journals a page in one transaction and
// replays it exactly as one-by-one appends would — same counter, same decoding.
func TestReactiveJournal_AppendSignalsPageReplaysInOrder(t *testing.T) {
	j, done := openDeliveryJournal(t, filepath.Join(t.TempDir(), "agents.db"))
	defer done()
	page := []domain.Signal{
		{StreamID: "ingress.raw.a", Payload: map[string]any{"n": "1"}},
		{StreamID: "ingress.raw.a", Payload: map[string]any{"n": "2"}},
	}
	seqs, err := j.AppendSignals(page, time.Hour)
	if err != nil || len(seqs) != 2 || seqs[1] != seqs[0]+1 {
		t.Fatalf("AppendSignals = %v, %v", seqs, err)
	}
	recs, err := j.ReplayStreams(nil, 0)
	if err != nil || len(recs) != 2 {
		t.Fatalf("replay: %d records, err %v", len(recs), err)
	}
	if recs[0].Seq != seqs[0] || recs[1].Signal.Payload["n"] != "2" {
		t.Fatalf("replayed %+v", recs)
	}
	if seqs, err := j.AppendSignals(nil, time.Hour); err != nil || seqs != nil {
		t.Fatalf("empty page: %v %v", seqs, err)
	}
}
