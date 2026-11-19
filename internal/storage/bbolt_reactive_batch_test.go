package storage

import (
	"fmt"
	"testing"
	"time"
)

// ENG-1292: a page of signals is journaled in one transaction. The seqs are the
// same journal-wide counter a one-by-one append uses, so replay and cursors cannot
// tell the two apart; the page interleaves with single appends without a gap.
func TestReactive_AppendSignalsBatchSharesTheOneCounter(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	first := appendOn(t, a, "live", time.Time{})
	page := []ReactiveJournalRecord{
		{StreamID: "fill", SignalJSON: []byte(`{"n":1}`), ReceivedAt: time.Now().UTC()},
		{StreamID: "fill", SignalJSON: []byte(`{"n":2}`), ReceivedAt: time.Now().UTC()},
		{StreamID: "other", SignalJSON: []byte(`{"n":3}`), ReceivedAt: time.Now().UTC()},
	}
	seqs, err := a.AppendReactiveSignals(page)
	if err != nil {
		t.Fatal(err)
	}
	last := appendOn(t, a, "live", time.Time{})
	want := []uint64{first + 1, first + 2, first + 3}
	if fmt.Sprint(seqs) != fmt.Sprint(want) || last != first+4 {
		t.Fatalf("batch seqs %v (want %v), next single %d (want %d)", seqs, want, last, first+4)
	}
	fill, err := a.ReplayReactiveStreams(exact("fill"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(fill); fmt.Sprint(got) != fmt.Sprint(want[:2]) || string(fill[1].SignalJSON) != `{"n":2}` {
		t.Fatalf("fill stream replay = %v %q", got, fill)
	}
	if seqs, err := a.AppendReactiveSignals(nil); err != nil || seqs != nil {
		t.Fatalf("empty page: %v %v", seqs, err)
	}
}

// BenchmarkReactiveJournalAppend measures the cost ENG-1292 adds to a history fill:
// a 500-record page journaled one record per transaction (as the live transports
// append) against one transaction per page. Local, no LLM.
//
//	go test -run '^$' -bench BenchmarkReactiveJournalAppend -benchtime 3x ./internal/storage/
func BenchmarkReactiveJournalAppend(b *testing.B) {
	const pageSize = 500
	rec := ReactiveJournalRecord{StreamID: "ingress.raw.bench", ReceivedAt: time.Now().UTC(),
		SignalJSON: []byte(`{"stream_id":"ingress.raw.bench","payload":{"ingress_id":"bench","cid":"bafkreia","delivery_ref":"order-000001","content_type":"application/json","received_at":"2026-11-20T00:00:00Z","namespace":"default","lane":"backfill","backfill_id":"bf_1"}}`)}
	page := make([]ReactiveJournalRecord, pageSize)
	for i := range page {
		page[i] = rec
	}
	b.Run("per-record", func(b *testing.B) {
		a := benchJournal(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, r := range page {
				if _, err := a.AppendReactiveSignal(r); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*pageSize), "µs/record")
	})
	b.Run("per-page", func(b *testing.B) {
		a := benchJournal(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := a.AppendReactiveSignals(page); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*pageSize), "µs/record")
	})
}

func benchJournal(b *testing.B) *BBoltAdapter {
	b.Helper()
	a, err := NewBBoltAdapterNoScan(b.TempDir() + "/j.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { a.db.Close() })
	return a
}
