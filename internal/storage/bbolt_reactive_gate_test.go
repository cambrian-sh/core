package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ADR-0061 Amendment A2 gate — the journal micro-benchmark.
//
// The three gate criteria, A/B against the pre-A2 flat layout:
//
//  1. Boot replay: 1 M journaled records on one stream plus 17 watches on other,
//     idle streams; replay must complete in under 1 s.
//  2. Append throughput at 1, 8 and 32 concurrent producers: no regression.
//  3. Bounded disk: sustained 100 signals/s for 2 h with TTL 1 h and an idle
//     watch on another stream; live records stay at about one TTL's worth, and
//     the file after one compaction is within 2x of the live data.
//
// The runs are opt-in because they cost disk and minutes (and far more under
// -race):
//
//	CAMBRIAN_JOURNAL_GATE=1      full size (1 M records; ~1 GB of temp files)
//	CAMBRIAN_JOURNAL_GATE=small  reduced size, same shape
//	CAMBRIAN_JOURNAL_GATE_A=1    also run the A arm of the replay at full size
//	                             (it is the minutes-long boot stall itself)
//
// Unset, only the B arm of the replay runs (50 k records, well under a second),
// so the default suite still asserts that idle watches read nothing. Results are
// reported with t.Logf as `GATE ...` lines; run with -v to see them.
//
// The "A" arm is a faithful copy of the pre-A2 code paths (flat bucket, whole
// record as JSON with the signal JSON nested inside, one db.Update per append,
// ReplayReactiveFrom per watch with the stream filter applied after decoding).
// It lives only in this test file.

func gateFull() bool { return os.Getenv("CAMBRIAN_JOURNAL_GATE") == "1" }

func gateOn() bool { return os.Getenv("CAMBRIAN_JOURNAL_GATE") != "" }

func requireGate(t *testing.T) {
	t.Helper()
	if !gateOn() {
		t.Skip("journal gate: set CAMBRIAN_JOURNAL_GATE=1 (full) or =small to run")
	}
}

// gateSignal is a realistic signal body (~600 B, the shape of an ingress-studio
// raw delivery reference).
func gateSignal(stream string, i int) []byte {
	sig := map[string]any{
		"stream_id": stream,
		"timestamp": time.Unix(1762000000, int64(i)).UTC().Format(time.RFC3339Nano),
		"raw_text":  "delivery reference " + strconv.Itoa(i),
		"payload": map[string]any{
			"delivery_id": fmt.Sprintf("dlv-%012d", i),
			"ingress_id":  "ingress-studio-raw",
			"source":      "imap://mail.example.internal/INBOX",
			"content_ref": fmt.Sprintf("cas://sha256/%064x", i),
			"headers":     map[string]any{"from": "someone@example.com", "subject": "a subject line of ordinary length"},
			"received_at": time.Unix(1762000000, int64(i)).UTC().Format(time.RFC3339Nano),
			"tags":        []string{"tenant:default", "class:raw", "lane:ingress"},
		},
	}
	b, _ := json.Marshal(sig)
	return b
}

// ── the pre-A2 layout, copied from the code A2 replaced ────────────────────────

func legacyAppend(db *bbolt.DB, rec ReactiveJournalRecord) (uint64, error) {
	var seq uint64
	err := db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(reactiveJournalLegacyBucket)
		if err != nil {
			return err
		}
		n, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		seq = n
		rec.Seq = seq
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put(itob(seq), data)
	})
	return seq, err
}

func legacyReplayFrom(db *bbolt.DB, afterSeq uint64) ([]ReactiveJournalRecord, error) {
	var recs []ReactiveJournalRecord
	err := db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveJournalLegacyBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Seek(itob(afterSeq + 1)); k != nil; k, v = c.Next() {
			var rec ReactiveJournalRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			recs = append(recs, rec)
		}
		return nil
	})
	return recs, err
}

// signalStream is what the kernel decorator did next: decode the signal JSON.
func signalStream(raw []byte) string {
	var s struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.StreamID
}

// ── fixtures ───────────────────────────────────────────────────────────────────

// bulkLoad writes n records on one stream in large transactions, with NoSync —
// a fixture, not a measurement. layout "A" writes the legacy flat bucket, "B" the
// A2 per-stream bucket. The 17 idle watches' streams got one early record each.
func bulkLoad(t testing.TB, path, layout, hot string, idle []string, n int) {
	t.Helper()
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ttl := time.Now().Add(-time.Hour) // every record TTL-expired, as on 2026-11-02
	put := func(tx *bbolt.Tx, seq uint64, stream string, i int) error {
		rec := ReactiveJournalRecord{Seq: seq, StreamID: stream, SignalJSON: gateSignal(stream, i), ReceivedAt: ttl.Add(-time.Hour), TTLExpires: ttl}
		if layout == "A" {
			b, _ := tx.CreateBucketIfNotExists(reactiveJournalLegacyBucket)
			data, _ := json.Marshal(rec)
			if err := b.Put(itob(seq), data); err != nil {
				return err
			}
			return b.SetSequence(seq)
		}
		parent, _ := tx.CreateBucketIfNotExists(reactiveJournalBucket)
		sb, _ := parent.CreateBucketIfNotExists(streamBucketName(stream))
		sb.FillPercent = 1.0
		if err := sb.Put(itob(seq), encodeJournalValue(rec)); err != nil {
			return err
		}
		return parent.SetSequence(seq)
	}
	seq := uint64(0)
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, s := range idle {
			seq++
			if err := put(tx, seq, s, int(seq)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const chunk = 50000
	for done := 0; done < n; {
		if err := db.Update(func(tx *bbolt.Tx) error {
			for i := 0; i < chunk && done < n; i++ {
				seq++
				done++
				if err := put(tx, seq, hot, done); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
}

func idleStreams() []string {
	out := make([]string, 17)
	for i := range out {
		out[i] = fmt.Sprintf("rxbench-idle-%02d", i)
	}
	return out
}

// ── gate 1: boot replay ───────────────────────────────────────────────────────

// TestJournalGate_BootReplay measures what the engine's replay() does at boot for
// 17 watches on idle streams (the 2026-11-02 shape: rxbench-* watches whose
// streams went quiet after a few signals, cursors parked at the start of the
// journal), with the hot stream holding n records no replaying watch reads. Each
// idle stream got one early record, which its watch has acked: the cursor is 17.
//
//	A: one ReplayReactiveFrom(cursor) per watch over the flat bucket, decoding
//	   every record twice (the record, then the signal) and filtering by stream.
//	B: one ReplayReactiveStreams(exact(stream), cursor) per watch group.
func TestJournalGate_BootReplay(t *testing.T) {
	n := 50000
	if gateFull() {
		n = 1000000
	}
	idle := idleStreams()
	cursor := uint64(len(idle))

	measure := func(layout string, n int) (time.Duration, int, int64) {
		path := filepath.Join(t.TempDir(), "replay-"+layout+".db")
		bulkLoad(t, path, layout, "ingress-studio-raw", idle, n)
		st, _ := os.Stat(path)
		db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		a := &BBoltAdapter{db: db}
		start := time.Now()
		dispatched := 0
		for _, stream := range idle {
			if layout == "A" {
				recs, err := legacyReplayFrom(db, cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range recs {
					if signalStream(r.SignalJSON) == stream {
						dispatched++
					}
				}
				continue
			}
			recs, err := a.ReplayReactiveStreams(exact(stream), cursor)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range recs {
				if signalStream(r.SignalJSON) == stream {
					dispatched++
				}
			}
		}
		return time.Since(start), dispatched, st.Size()
	}

	dB, nB, sizeB := measure("B", n)
	t.Logf("GATE replay B (A2 per-stream): records=%d idle_watches=%d duration=%s dispatched=%d file=%d B", n, len(idle), dB, nB, sizeB)
	if nB != 0 {
		t.Fatalf("idle watches at cursor %d dispatched %d records; their streams' records are all at seq <= %d", cursor, nB, len(idle))
	}
	if gateFull() && dB >= time.Second {
		t.Fatalf("GATE 1 FAILED: replay took %s at %d records (< 1 s required)", dB, n)
	}
	// The A arm at 1 M x 17 watches is the minutes-long boot stall itself (2m59s
	// on the 2026-11-03 gate run); it runs at full size only on request, and in
	// the default suite at a tenth of the reduced size so the comparison is
	// still stated without costing the suite seconds.
	if (gateOn() && !gateFull()) || os.Getenv("CAMBRIAN_JOURNAL_GATE_A") == "1" {
		nA := n
		if !gateFull() {
			nA = n / 10
		}
		dA, dispatchedA, sizeA := measure("A", nA)
		t.Logf("GATE replay A (pre-A2 flat): records=%d idle_watches=%d duration=%s dispatched=%d file=%d B", nA, len(idle), dA, dispatchedA, sizeA)
	}
}

// ── gate 2: append throughput ─────────────────────────────────────────────────

// TestJournalGate_AppendThroughput appends with 1, 8 and 32 concurrent producers
// through each layout's real durable path (fsync on) and reports signals/s.
func TestJournalGate_AppendThroughput(t *testing.T) {
	requireGate(t)
	perProducerTotal := 400
	if gateFull() {
		perProducerTotal = 2000
	}
	for _, producers := range []int{1, 8, 32} {
		total := perProducerTotal
		if producers == 1 {
			total = perProducerTotal / 2
		}
		for _, layout := range []string{"A", "B"} {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("append-%s-%d.db", layout, producers))
			a, cleanup := openReactiveTestAdapter(t, path)
			if layout == "A" {
				_ = a.db.Update(func(tx *bbolt.Tx) error {
					_, err := tx.CreateBucketIfNotExists(reactiveJournalLegacyBucket)
					return err
				})
			}
			sig := gateSignal("ingress-studio-raw", 1)
			var wg sync.WaitGroup
			per := total / producers
			if per == 0 {
				per = 1
			}
			start := time.Now()
			for p := 0; p < producers; p++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < per; i++ {
						rec := ReactiveJournalRecord{StreamID: "ingress-studio-raw", SignalJSON: sig, ReceivedAt: time.Now(), TTLExpires: time.Now().Add(time.Hour)}
						var err error
						if layout == "A" {
							_, err = legacyAppend(a.db, rec)
						} else {
							_, err = a.AppendReactiveSignal(rec)
						}
						if err != nil {
							t.Errorf("append: %v", err)
							return
						}
					}
				}()
			}
			wg.Wait()
			d := time.Since(start)
			cleanup()
			t.Logf("GATE append %s producers=%d appends=%d duration=%s rate=%.0f/s", layout, producers, per*producers, d, float64(per*producers)/d.Seconds())
		}
	}
}

// ── gate 3: bounded disk ──────────────────────────────────────────────────────

// TestJournalGate_BoundedDisk simulates sustained ingest on a clock: 100 signals/s
// for 2 h (720 k records) with TTL 1 h, a GC pass every simulated minute, and an
// idle watch at cursor 0 on another stream that got one record at the start. The
// hot stream has no consumer holding it back beyond TTL. Appends are grouped one
// transaction per simulated second with NoSync — this measures retention and
// file size, not fsync cost.
func TestJournalGate_BoundedDisk(t *testing.T) {
	requireGate(t)
	rate, hours := 100, 2.0
	if !gateFull() {
		rate, hours = 10, 2.0
	}
	ttl := time.Hour
	path := filepath.Join(t.TempDir(), "disk.db")
	a, cleanup := openReactiveTestAdapter(t, path)
	a.db.NoSync = true
	base := time.Unix(1762000000, 0).UTC()
	seconds := int(hours * 3600)

	// The idle stream's single record, never acked by its watch (cursor 0).
	idleSeq, _ := a.AppendReactiveSignal(ReactiveJournalRecord{StreamID: "idle", SignalJSON: gateSignal("idle", 0), ReceivedAt: base, TTLExpires: base.Add(ttl)})
	floor := func(s string) (uint64, bool) {
		if s == "idle" {
			return 0, true
		}
		return 0, false // the hot stream's watch is fully caught up: TTL governs
	}
	peak := 0
	i := 0
	for sec := 0; sec < seconds; sec++ {
		now := base.Add(time.Duration(sec) * time.Second)
		if err := a.db.Update(func(tx *bbolt.Tx) error {
			parent := tx.Bucket(reactiveJournalBucket)
			sb, _ := parent.CreateBucketIfNotExists([]byte("hot"))
			sb.FillPercent = 1.0
			for k := 0; k < rate; k++ {
				i++
				seq, _ := parent.NextSequence()
				if err := sb.Put(itob(seq), encodeJournalValue(ReactiveJournalRecord{SignalJSON: gateSignal("hot", i), ReceivedAt: now, TTLExpires: now.Add(ttl)})); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if sec%60 == 59 {
			for {
				_, more, err := a.PruneReactiveJournal(floor, now, 5000)
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
			if _, _, c, _ := a.ReactiveJournalWindow(nil); c > peak {
				peak = c
			}
		}
	}
	_, _, live, _ := a.ReactiveJournalWindow(nil)
	var liveBytes int64
	_ = a.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(reactiveJournalBucket).ForEachBucket(func(name []byte) error {
			return tx.Bucket(reactiveJournalBucket).Bucket(name).ForEach(func(k, v []byte) error {
				liveBytes += int64(len(k) + len(v))
				return nil
			})
		})
	})
	idleLeft, _ := a.ReplayReactiveStreams(exact("idle"), 0)
	_ = a.db.Sync()
	cleanup()
	before, _ := os.Stat(path)

	oneTTL := rate * int(ttl/time.Second)
	t.Logf("GATE disk: rate=%d/s hours=%.1f appended=%d live_records=%d peak_live=%d one_ttl=%d live_bytes=%d file_before_compact=%d B",
		rate, hours, i, live, peak, oneTTL, liveBytes, before.Size())
	if len(idleLeft) != 1 || idleLeft[0].Seq != idleSeq {
		t.Fatal("the idle watch's un-acked record was pruned")
	}
	// About one TTL's worth (+ one GC interval of arrivals, + the idle record).
	if limit := oneTTL + 60*rate + 1; peak > limit {
		t.Fatalf("GATE 3 FAILED: peak live %d > one TTL + one GC interval (%d)", peak, limit)
	}

	rep, err := CompactBBoltFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(rep.AfterBytes) / float64(liveBytes)
	t.Logf("GATE disk: compacted %d -> %d B (%.2fx live data)", rep.FileBytes, rep.AfterBytes, ratio)
	if ratio > 2 {
		t.Fatalf("GATE 3 FAILED: compacted file is %.2fx the live data (<= 2x required)", ratio)
	}
}
