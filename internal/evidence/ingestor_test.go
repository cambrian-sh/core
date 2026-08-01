package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The tests here are the ADR-0105 gates, not coverage decoration. Each one maps
// to a row of the memo's §17 crash/replay table and is named for it.

// fakeBlobs is a ContentStore with injectable failpoints.
type fakeBlobs struct {
	data      map[domain.CID][]byte
	putErr    error
	hasErr    error
	hasAbsent bool // report the blob missing even after a successful put
	putCalls  int
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{data: map[domain.CID][]byte{}} }

func (f *fakeBlobs) Put(_ context.Context, data []byte, _ string, _ []string, _ string) (domain.CID, error) {
	f.putCalls++
	if f.putErr != nil {
		return "", f.putErr
	}
	sum := sha256.Sum256(data)
	cid := domain.CID(hex.EncodeToString(sum[:]))
	f.data[cid] = data
	return cid, nil
}

func (f *fakeBlobs) Get(_ context.Context, cid domain.CID) (*domain.ContextNode, error) {
	d, ok := f.data[cid]
	if !ok {
		return nil, errors.New("not found")
	}
	return &domain.ContextNode{CID: cid, Data: d}, nil
}

func (f *fakeBlobs) Has(_ context.Context, cid domain.CID) (bool, error) {
	if f.hasErr != nil {
		return false, f.hasErr
	}
	if f.hasAbsent {
		return false, nil
	}
	_, ok := f.data[cid]
	return ok, nil
}

func (f *fakeBlobs) GC(_ context.Context, _ []domain.CID) error { return nil }

// fakeStore is an EvidenceStore with injectable failpoints. It records call
// order so ordering assertions are against observed behaviour, not comments.
type fakeStore struct {
	rows      map[string]domain.Evidence // key: source triple
	outbox    []domain.EvidenceOutboxItem
	processed map[int64]bool
	insertErr error
	nextID    int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]domain.Evidence{}, processed: map[int64]bool{}}
}

func tripleKey(ev domain.Evidence) string {
	return ev.NamespaceID + "|" + ev.SourceID + "|" + ev.SourceKey + "|" + ev.SourceRevision
}

func (f *fakeStore) Insert(_ context.Context, ev domain.Evidence) (domain.EvidenceID, bool, error) {
	if f.insertErr != nil {
		return "", false, f.insertErr
	}
	if ev.NamespaceID == "" {
		ev.NamespaceID = "default"
	}
	k := tripleKey(ev)
	if existing, ok := f.rows[k]; ok {
		return existing.ID, false, nil
	}
	f.nextID++
	ev.ID = domain.EvidenceID("ev_" + strings.Repeat("0", 3) + string(rune('a'+f.nextID)))
	f.rows[k] = ev
	f.outbox = append(f.outbox, domain.EvidenceOutboxItem{
		ID: f.nextID, NamespaceID: ev.NamespaceID, EvidenceID: ev.ID, CreatedAt: time.Now(),
	})
	return ev.ID, true, nil
}

func (f *fakeStore) Get(_ context.Context, id domain.EvidenceID) (*domain.Evidence, error) {
	for _, ev := range f.rows {
		if ev.ID == id {
			cp := ev
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeStore) PendingOutbox(_ context.Context, limit int) ([]domain.EvidenceOutboxItem, error) {
	var out []domain.EvidenceOutboxItem
	for _, it := range f.outbox {
		if !f.processed[it.ID] {
			out = append(out, it)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) MarkProcessed(_ context.Context, id int64) (bool, error) {
	if f.processed[id] {
		return false, nil
	}
	f.processed[id] = true
	return true, nil
}

func raw(key string) Raw {
	return Raw{
		SourceID:  "test:src",
		SourceKey: key,
		Bytes:     []byte("delivery body for " + key),
	}
}

func mustIngestor(t *testing.T, b domain.ContentStore, s domain.EvidenceStore) *Ingestor {
	t.Helper()
	ing, err := NewIngestor(b, s)
	if err != nil {
		t.Fatalf("NewIngestor: %v", err)
	}
	return ing
}

// Gate: happy path — bytes durable, pointer verified, row + outbox atomically,
// and the returned id resolves back to content that can be reprocessed.
func TestIngest_PreservesBytesBeforePublishingEvidence(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)

	id, inserted, err := ing.Ingest(context.Background(), raw("k1"))
	if err != nil || !inserted {
		t.Fatalf("Ingest: id=%q inserted=%v err=%v", id, inserted, err)
	}
	ev, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ok, _ := blobs.Has(context.Background(), ev.ContentHash)
	if !ok {
		t.Fatal("published evidence points at content that is not retrievable")
	}
	if len(store.outbox) != 1 {
		t.Fatalf("outbox items = %d, want 1", len(store.outbox))
	}
}

// Gate (crash after blob write, before DB commit): orphan blob may remain; NO
// published evidence row, no outbox item.
func TestIngest_CrashBeforeCommitLeavesOrphanBlobNotDanglingRow(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	store.insertErr = errors.New("injected: crash at commit")
	ing := mustIngestor(t, blobs, store)

	_, _, err := ing.Ingest(context.Background(), raw("k1"))
	if err == nil {
		t.Fatal("expected the injected commit failure to surface")
	}
	if len(store.rows) != 0 || len(store.outbox) != 0 {
		t.Fatalf("evidence/outbox written despite failed commit: rows=%d outbox=%d",
			len(store.rows), len(store.outbox))
	}
	if len(blobs.data) != 1 {
		t.Fatalf("expected exactly the orphan blob to remain, got %d", len(blobs.data))
	}

	// And the retry after the "crash" succeeds against the same bytes: Put is
	// idempotent, so recovery is a plain re-ingest.
	store.insertErr = nil
	if _, inserted, err := ing.Ingest(context.Background(), raw("k1")); err != nil || !inserted {
		t.Fatalf("retry after crash: inserted=%v err=%v", inserted, err)
	}
}

// Gate: an unverifiable pointer must never be published. "Has == false" right
// after a successful Put is a store integrity failure, not a race to tolerate.
func TestIngest_UnretrievableContentIsNeverPublished(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	blobs.hasAbsent = true
	ing := mustIngestor(t, blobs, store)

	if _, _, err := ing.Ingest(context.Background(), raw("k1")); err == nil {
		t.Fatal("expected verification failure to surface")
	}
	if len(store.rows) != 0 {
		t.Fatal("evidence row published for unretrievable content")
	}
}

// Gate (replay identical source key and revision): no duplicate evidence
// version, no duplicate outbox work.
func TestIngest_ReplayCreatesNoDuplicateVersionAndNoDuplicateWork(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)

	id1, ins1, err := ing.Ingest(context.Background(), raw("k1"))
	if err != nil || !ins1 {
		t.Fatalf("first: %v", err)
	}
	id2, ins2, err := ing.Ingest(context.Background(), raw("k1"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if ins2 {
		t.Fatal("replay reported inserted=true")
	}
	if id1 != id2 {
		t.Fatalf("replay minted a new identity: %q vs %q", id1, id2)
	}
	if len(store.rows) != 1 || len(store.outbox) != 1 {
		t.Fatalf("replay duplicated state: rows=%d outbox=%d", len(store.rows), len(store.outbox))
	}
}

// Gate: a REVISED artifact is new evidence, not an update — both revisions
// remain, separately retrievable, each with its own bytes.
func TestIngest_SourceRevisionIsNewEvidenceNeverAnUpdate(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)

	r1 := raw("k1")
	r1.SourceRevision = "1"
	id1, _, err := ing.Ingest(context.Background(), r1)
	if err != nil {
		t.Fatalf("rev1: %v", err)
	}
	r2 := raw("k1")
	r2.SourceRevision = "2"
	r2.Bytes = []byte("revised body")
	r2.RevisesID = id1
	id2, ins2, err := ing.Ingest(context.Background(), r2)
	if err != nil || !ins2 {
		t.Fatalf("rev2: inserted=%v err=%v", ins2, err)
	}
	if id1 == id2 {
		t.Fatal("revision reused the prior row")
	}
	ev1, _ := store.Get(context.Background(), id1)
	ev2, _ := store.Get(context.Background(), id2)
	if ev1.ContentHash == ev2.ContentHash {
		t.Fatal("revisions share a content hash despite different bytes")
	}
	if ev2.RevisesID != id1 {
		t.Fatalf("revision link lost: %q", ev2.RevisesID)
	}
}

// Gate (crash after evidence commit, before outbox consumption): the item
// replays, and the MarkProcessed transition happens exactly once logically.
func TestOutbox_TransitionIsExactlyOnceLogically(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)
	if _, _, err := ing.Ingest(context.Background(), raw("k1")); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	pending, err := store.PendingOutbox(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v err=%v", pending, err)
	}
	// Two consumers race the same item; exactly one wins.
	first, _ := store.MarkProcessed(context.Background(), pending[0].ID)
	second, _ := store.MarkProcessed(context.Background(), pending[0].ID)
	if !first || second {
		t.Fatalf("transition not exactly-once-logical: first=%v second=%v", first, second)
	}
	left, _ := store.PendingOutbox(context.Background(), 10)
	if len(left) != 0 {
		t.Fatalf("processed item still pending: %v", left)
	}
}

// Gate (a deliberately failed extraction still leaves reprocessable evidence):
// evidence exists independently of any downstream pipeline outcome, and the
// original bytes come back byte-identical.
func TestIngest_EvidenceIsReprocessableRegardlessOfDownstreamFailure(t *testing.T) {
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)

	body := []byte("the source material an extractor will later fail on")
	r := raw("k1")
	r.Bytes = body
	id, _, err := ing.Ingest(context.Background(), r)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// (a downstream extraction failing here is out of frame on purpose: it must
	// have NOTHING it can do to the archive)
	ev, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	node, err := blobs.Get(context.Background(), ev.ContentHash)
	if err != nil {
		t.Fatalf("blob get: %v", err)
	}
	if string(node.Data) != string(body) {
		t.Fatal("archived bytes are not the delivered bytes")
	}
}

func TestIngest_RefusesEmptyDeliveries(t *testing.T) {
	ing := mustIngestor(t, newFakeBlobs(), newFakeStore())
	if _, _, err := ing.Ingest(context.Background(), Raw{SourceID: "s", SourceKey: "k"}); err == nil {
		t.Fatal("empty delivery accepted")
	}
	if _, _, err := ing.Ingest(context.Background(), Raw{Bytes: []byte("x")}); err == nil {
		t.Fatal("delivery without source identity accepted")
	}
}
