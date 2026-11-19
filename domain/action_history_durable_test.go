package domain

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// memActionHistoryStore is an in-memory ActionHistoryStore with the Postgres
// adapter's semantics (upsert by key, prune on write, window-bounded load).
type memActionHistoryStore struct {
	mu      sync.Mutex
	rows    map[string]memHistRow
	failPut error
	failGet error
	loads   int
}

type memHistRow struct {
	scope string
	rec   ActionRecord
}

func newMemActionHistoryStore() *memActionHistoryStore {
	return &memActionHistoryStore{rows: map[string]memHistRow{}}
}

func (s *memActionHistoryStore) RecordDone(_ context.Context, scope string, rec ActionRecord, key string, keepAfter time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil {
		return s.failPut
	}
	rec.Args = append([]byte(nil), rec.Args...)
	s.rows[key] = memHistRow{scope: scope, rec: rec}
	for k, r := range s.rows {
		if r.rec.At.Before(keepAfter) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *memActionHistoryStore) LoadDone(_ context.Context, scope string, since time.Time, limit int) ([]ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.failGet != nil {
		return nil, s.failGet
	}
	var out []ActionRecord
	for _, r := range s.rows {
		if r.scope == scope && r.rec.At.After(since) {
			out = append(out, r.rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ENG-784: a done call outlives the process. A second history over the same store
// (the restart) matches the earlier call with its approval and ids; a call that
// never finished (dropped, or still pending) does not persist.
func TestActionHistory_DoneCallsSurviveARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemActionHistoryStore()
	clock := &stepClock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	h1 := NewDurableActionHistory(time.Hour, clock.now, store)
	args := []byte(`{"project_key":"OPS","summary":"INC-1040 (Ashvale) - Database outage"}`)
	ref := h1.Begin("conversation:c1", "create_issue", args, ActionAwaitingApproval)
	h1.Approved(ref, "apr-1")
	h1.Finish(ctx, ref, []string{"key=OPS-11"})
	h1.Drop(h1.Begin("conversation:c1", "create_issue", []byte(`{"project_key":"OPS","summary":"failed"}`), ActionRunning))
	h1.Begin("conversation:c1", "send_mail", []byte(`{"to":"a@x.com","subject":"pending"}`), ActionAwaitingApproval)
	if len(store.rows) != 1 {
		t.Fatalf("persisted rows = %d, want only the done call", len(store.rows))
	}

	clock.advance(10 * time.Minute)
	h2 := NewDurableActionHistory(time.Hour, clock.now, store) // the restart
	got := h2.Matches(ctx, "conversation:c1", "create_issue",
		[]byte(`{"project_key":"OPS","summary":"INC-1040 - Database outage"}`), ActionRef{})
	if len(got) != 1 || got[0].Prior.ApprovalID != "apr-1" || got[0].Prior.State != ActionDone ||
		strings.Join(got[0].Prior.IDs, ",") != "key=OPS-11" {
		t.Fatalf("after restart: %+v", got)
	}
	notice := RenderDuplicateNotice(got)
	if !strings.Contains(notice, "approval apr-1") || !strings.Contains(notice, "key=OPS-11") {
		t.Fatalf("notice = %q", notice)
	}
	if m := h2.Matches(ctx, "conversation:c1", "send_mail", []byte(`{"to":"a@x.com","subject":"pending"}`), ActionRef{}); len(m) != 0 {
		t.Fatalf("a call that never finished persisted: %+v", m)
	}
	if m := h2.Matches(ctx, "conversation:c2", "create_issue", args, ActionRef{}); len(m) != 0 {
		t.Fatalf("another conversation matched: %+v", m)
	}
	if store.loads != 2 {
		t.Fatalf("store loads = %d, want one per scope", store.loads)
	}
}

// A call finished in this process before its scope was first matched is not
// listed twice once the scope is read back (same content-derived key).
func TestActionHistory_ReadBackSkipsCallsThisProcessFinished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemActionHistoryStore()
	h := NewDurableActionHistory(time.Hour, nil, store)
	args := []byte(`{"to":"a@x.com","subject":"Report"}`)
	h.Finish(ctx, h.Begin("conversation:c1", "send_mail", args, ActionRunning), nil)
	if got := h.Matches(ctx, "conversation:c1", "send_mail", args, ActionRef{}); len(got) != 1 {
		t.Fatalf("matches = %d, want 1 (no duplicate from the read-back)", len(got))
	}
	if ActionRecordKey("conversation:c1", ActionRecord{Tool: "send_mail", Args: []byte(`{"subject":"report","to":"A@x.com"}`)}) !=
		ActionRecordKey("conversation:c1", ActionRecord{Tool: "send_mail", Args: args}) {
		t.Fatal("the key must follow the canonical arguments, not their spelling")
	}
	if ActionRecordKey("conversation:c1", ActionRecord{Tool: "send_mail", Args: args}) ==
		ActionRecordKey("conversation:c2", ActionRecord{Tool: "send_mail", Args: args}) {
		t.Fatal("the key must include the scope")
	}
}

// Outside the window a persisted call is neither read back nor kept.
func TestActionHistory_PersistedCallsExpireWithTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemActionHistoryStore()
	clock := &stepClock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	h1 := NewDurableActionHistory(time.Hour, clock.now, store)
	args := []byte(`{"to":"a@x.com","subject":"Report"}`)
	h1.Finish(ctx, h1.Begin("conversation:c1", "send_mail", args, ActionRunning), nil)
	clock.advance(2 * time.Hour)
	h2 := NewDurableActionHistory(time.Hour, clock.now, store)
	if got := h2.Matches(ctx, "conversation:c1", "send_mail", args, ActionRef{}); len(got) != 0 {
		t.Fatalf("an expired call was read back: %+v", got)
	}
	h2.Finish(ctx, h2.Begin("conversation:c9", "post", []byte(`{"x":1}`), ActionRunning), nil)
	if len(store.rows) != 1 {
		t.Fatalf("rows after a write = %d, want the expired one pruned", len(store.rows))
	}
}

// The notice is a hint: a store that fails to write or read leaves the history
// working in memory, and a failed read is retried on the next match.
func TestActionHistory_StoreErrorsDegradeToMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemActionHistoryStore()
	store.failPut = errors.New("db down")
	h := NewDurableActionHistory(time.Hour, nil, store)
	args := []byte(`{"to":"a@x.com","subject":"Report"}`)
	h.Finish(ctx, h.Begin("conversation:c1", "send_mail", args, ActionRunning), nil)
	store.failGet = errors.New("db down")
	if got := h.Matches(ctx, "conversation:c1", "send_mail", args, ActionRef{}); len(got) != 1 {
		t.Fatalf("in-memory match lost on store errors: %+v", got)
	}
	store.failGet = nil
	h.Matches(ctx, "conversation:c1", "send_mail", args, ActionRef{})
	if store.loads != 2 {
		t.Fatalf("loads = %d, want the failed read retried", store.loads)
	}
}

// End to end through the executor: the first kernel run's approved write is
// named in the approval the second run (a new executor and history over the same
// store) raises for the same write.
func TestExecute_DuplicateNoticeSurvivesARestart(t *testing.T) {
	t.Parallel()
	store := newMemActionHistoryStore()
	ctx := WithConversationID(context.Background(), "conv-1")
	run := func() *recordingApproval {
		appr := &recordingApproval{approve: true}
		e := dupExec(appr, &fakeHandler{result: []byte(`{"key":"OPS-11"}`)})
		e.History = NewDurableActionHistory(24*time.Hour, nil, store)
		if r := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue",
			ArgsJSON: []byte(`{"project_key":"OPS","summary":"INC-1040 - Database outage"}`)}); r.Denied || r.Error != "" {
			t.Fatalf("call: %+v", r)
		}
		return appr
	}
	if first := run(); first.reqs[0].DuplicateNotice != "" {
		t.Fatalf("first run approval has a notice: %q", first.reqs[0].DuplicateNotice)
	}
	second := run() // the restart
	if !strings.HasPrefix(second.reqs[0].DuplicateNotice, DuplicateNoticePrefix) ||
		!strings.Contains(second.reqs[0].DuplicateNotice, "key=OPS-11") {
		t.Fatalf("second run's notice = %q", second.reqs[0].DuplicateNotice)
	}
}
