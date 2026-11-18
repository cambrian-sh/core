package erasureworker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type fakeOutbox struct {
	mu        sync.Mutex
	rows      []domain.ErasureOutboxRow
	shared    map[string]bool
	done      map[int64]bool
	failed    map[int64]string
	outcome   map[int64]string
	completed int
	prunedAt  time.Time
	erased    map[string]domain.ErasedRecords
}

func (f *fakeOutbox) PendingSteps(_ context.Context, limit, maxAttempts int) ([]domain.ErasureOutboxRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.ErasureOutboxRow
	for _, r := range f.rows {
		if !f.done[r.ID] && r.Attempts < maxAttempts && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeOutbox) CIDStillReferenced(_ context.Context, cid string) (bool, error) {
	return f.shared[cid], nil
}
func (f *fakeOutbox) VaultBodyStillReferenced(_ context.Context, hash string) (bool, error) {
	return f.shared[hash], nil
}
func (f *fakeOutbox) ErasedRecords(_ context.Context, requestID string) (domain.ErasedRecords, error) {
	if f.erased == nil {
		return domain.ErasedRecords{RequestID: requestID}, nil
	}
	return f.erased[requestID], nil
}
func (f *fakeOutbox) MarkStepDone(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done[id] {
		return false, nil
	}
	f.done[id] = true
	return true, nil
}
func (f *fakeOutbox) MarkStepFailed(_ context.Context, id int64, class string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = class
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Attempts++
		}
	}
	return nil
}
func (f *fakeOutbox) MarkStepNotErasable(_ context.Context, id int64, outcome string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done[id] {
		return false, nil
	}
	f.done[id] = true
	if f.outcome == nil {
		f.outcome = map[int64]string{}
	}
	f.outcome[id] = outcome
	return true, nil
}
func (f *fakeOutbox) CompleteRequests(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed++
	return 1, nil
}
func (f *fakeOutbox) PruneTombstones(_ context.Context, cutoff time.Time) (int, error) {
	f.prunedAt = cutoff
	return 2, nil
}

type fakeCAS struct {
	mu      sync.Mutex
	deleted []domain.CID
	fail    bool
}

func (c *fakeCAS) DeleteContent(_ context.Context, cid domain.CID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("disk")
	}
	c.deleted = append(c.deleted, cid)
	return nil
}

func newOutbox(rows ...domain.ErasureOutboxRow) *fakeOutbox {
	return &fakeOutbox{rows: rows, shared: map[string]bool{}, done: map[int64]bool{}, failed: map[int64]string{}}
}

// A cas_delete step deletes its blob unless another surviving record shares it; both
// mark the step done; requests are completed after the pass.
func TestRunOnce_DeletesUnsharedBlobsAndCompletes(t *testing.T) {
	ob := newOutbox(
		domain.ErasureOutboxRow{ID: 1, RequestID: "r", Step: domain.ErasureStepCASDelete, Ref: "cid-a"},
		domain.ErasureOutboxRow{ID: 2, RequestID: "r", Step: domain.ErasureStepCASDelete, Ref: "cid-shared"},
	)
	ob.shared["cid-shared"] = true
	cas := &fakeCAS{}
	w := New(ob, cas, Config{Poll: time.Hour}, nil)
	st := w.RunOnce(context.Background())
	if st.Done != 1 || st.Shared != 1 || st.Failed != 0 || st.Completed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if len(cas.deleted) != 1 || cas.deleted[0] != "cid-a" {
		t.Fatalf("deleted = %v, want only the unshared blob", cas.deleted)
	}
	if !ob.done[1] || !ob.done[2] {
		t.Fatal("both steps must be marked done")
	}
	if !ob.prunedAt.IsZero() {
		t.Fatal("retention 0 must keep tombstones forever")
	}
}

// Retries are bounded: a failing step is attempted MaxAttempts times, then left pending
// (its request reads overdue), never marked done. Unknown steps and a missing content
// store fail the same way.
func TestRunOnce_BoundedRetries(t *testing.T) {
	ob := newOutbox(
		domain.ErasureOutboxRow{ID: 1, RequestID: "r", Step: domain.ErasureStepCASDelete, Ref: "cid-a"},
		domain.ErasureOutboxRow{ID: 2, RequestID: "r", Step: "mystery", Ref: "x"},
	)
	w := New(ob, &fakeCAS{fail: true}, Config{Poll: time.Hour, MaxAttempts: 3}, nil)
	for i := 0; i < 5; i++ {
		w.RunOnce(context.Background())
	}
	if ob.done[1] || ob.done[2] {
		t.Fatal("a failed step must never be marked done")
	}
	if ob.rows[0].Attempts != 3 || ob.rows[1].Attempts != 3 {
		t.Fatalf("attempts = %d/%d, want 3/3", ob.rows[0].Attempts, ob.rows[1].Attempts)
	}
	if ob.failed[1] != "cas_delete_failed" || ob.failed[2] != "unknown_step" {
		t.Fatalf("error classes = %v", ob.failed)
	}
	noCAS := newOutbox(domain.ErasureOutboxRow{ID: 9, Step: domain.ErasureStepCASDelete, Ref: "c"})
	New(noCAS, nil, Config{Poll: time.Hour}, nil).RunOnce(context.Background())
	if noCAS.done[9] || noCAS.failed[9] != "no_content_store" {
		t.Fatal("no content store must leave the step pending")
	}
}

func TestRunOnce_PrunesTombstonesPastRetention(t *testing.T) {
	ob := newOutbox()
	w := New(ob, &fakeCAS{}, Config{Poll: time.Hour, TombstoneRetention: 48 * time.Hour}, nil)
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	if st := w.RunOnce(context.Background()); st.Pruned != 2 {
		t.Fatalf("pruned = %d", st.Pruned)
	}
	if !ob.prunedAt.Equal(now.Add(-48 * time.Hour)) {
		t.Fatalf("cutoff = %v", ob.prunedAt)
	}
}

// One owner: Start drains at once (resume after restart), Stop waits; idempotent; no
// goroutine leaks (TestMain).
func TestStartStop_DrainsOnStartAndStopsClean(t *testing.T) {
	ob := newOutbox(domain.ErasureOutboxRow{ID: 1, RequestID: "r", Step: domain.ErasureStepCASDelete, Ref: "cid-a"})
	cas := &fakeCAS{}
	w := New(ob, cas, Config{Poll: 10 * time.Millisecond}, nil)
	w.Start(context.Background())
	w.Start(context.Background()) // idempotent
	deadline := time.Now().Add(2 * time.Second)
	for {
		ob.mu.Lock()
		d := ob.done[1]
		ob.mu.Unlock()
		if d {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the start pass never drained the outbox")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()
	w.Stop()                                                // idempotent
	New(ob, cas, Config{}, nil).Start(context.Background()) // Poll 0: no goroutine
}

type fakeVault struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeVault) Delete(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, hash)
	return nil
}

// ADR-0157 D5.6: a vault_delete step deletes an erased ledger entry's vault body
// unless a surviving ledger entry still names the same bytes; without a vault the
// step stays pending.
func TestRunOnce_VaultDeleteRespectsSurvivingEntries(t *testing.T) {
	ob := newOutbox(
		domain.ErasureOutboxRow{ID: 1, RequestID: "r", Step: domain.ErasureStepVaultDelete, Ref: "body-a"},
		domain.ErasureOutboxRow{ID: 2, RequestID: "r", Step: domain.ErasureStepVaultDelete, Ref: "body-shared"},
	)
	ob.shared["body-shared"] = true
	v := &fakeVault{}
	st := New(ob, &fakeCAS{}, Config{Poll: time.Hour, Vault: v}, nil).RunOnce(context.Background())
	if st.Done != 1 || st.Shared != 1 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(v.deleted) != 1 || v.deleted[0] != "body-a" {
		t.Fatalf("deleted = %v, want only the unshared body", v.deleted)
	}
	noVault := newOutbox(domain.ErasureOutboxRow{ID: 3, Step: domain.ErasureStepVaultDelete, Ref: "b"})
	New(noVault, &fakeCAS{}, Config{Poll: time.Hour}, nil).RunOnce(context.Background())
	if noVault.done[3] || noVault.failed[3] != "no_vault" {
		t.Fatal("no vault must leave the step pending")
	}
}
