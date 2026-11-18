package domain

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ADR-0154 (owner decision 2026-11-14, ENG-812 + ENG-1428): the lifecycle is
// forward only, Record is idempotent on the deterministic key.
func TestActionState_ForwardOnly(t *testing.T) {
	ok := [][2]ActionState{
		{ActionStateProposed, ActionStateApproved}, {ActionStateProposed, ActionStateDispatched},
		{ActionStateApproved, ActionStateDispatched}, {ActionStateDispatched, ActionStateAccepted},
		{ActionStateDispatched, ActionStateNotConfirmed}, {ActionStateAccepted, ActionStateConfirmed},
		{ActionStateAccepted, ActionStateNotConfirmed}, {ActionStateDispatched, ActionStateDispatched},
	}
	for _, p := range ok {
		if !p[0].CanAdvanceTo(p[1]) {
			t.Errorf("%s → %s refused", p[0], p[1])
		}
	}
	bad := [][2]ActionState{
		{ActionStateAccepted, ActionStateDispatched}, {ActionStateConfirmed, ActionStateNotConfirmed},
		{ActionStateApproved, ActionStateProposed}, {ActionStateProposed, ActionStateAccepted},
		{ActionStateNotConfirmed, ActionStateDispatched},
	}
	for _, p := range bad {
		if p[0].CanAdvanceTo(p[1]) {
			t.Errorf("%s → %s allowed", p[0], p[1])
		}
	}
}

func TestMemActionRecordStore_RecordIsIdempotentAndAdvanceChecksTheLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemActionRecordStore()
	first, created, err := s.Record(ctx, DurableAction{Key: "k1", Tool: "erp.create_order", RunID: "run-1", StepIndex: 2})
	if err != nil || !created || first.State != ActionStateProposed {
		t.Fatalf("Record = %+v created=%v err=%v", first, created, err)
	}
	again, created, err := s.Record(ctx, DurableAction{Key: "k1", Tool: "other"})
	if err != nil || created || again.Tool != "erp.create_order" {
		t.Fatalf("second Record = %+v created=%v err=%v; want the existing row unchanged", again, created, err)
	}
	if _, err := s.Advance(ctx, "k1", ActionStateAccepted, ActionUpdate{}); !errors.Is(err, ErrActionTransition) {
		t.Fatalf("proposed → accepted: err = %v, want ErrActionTransition", err)
	}
	if _, err := s.Advance(ctx, "k1", ActionStateDispatched, ActionUpdate{Error: "timeout"}); err != nil {
		t.Fatal(err)
	}
	res := &ToolCallResponse{ResultJSON: []byte(`{"id":"SO-7"}`)}
	got, err := s.Advance(ctx, "k1", ActionStateAccepted, ActionUpdate{Result: res})
	if err != nil || got.State != ActionStateAccepted || string(got.Result.ResultJSON) != `{"id":"SO-7"}` || got.Error != "" {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	res.ResultJSON[0] = 'X' // the store holds its own copy
	if g, _ := s.Get(ctx, "k1"); string(g.Result.ResultJSON) != `{"id":"SO-7"}` {
		t.Fatalf("stored result aliased the caller's: %s", g.Result.ResultJSON)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrActionRecordNotFound) {
		t.Fatalf("Get(unknown) = %v", err)
	}
	if _, err := s.Advance(ctx, "nope", ActionStateDispatched, ActionUpdate{}); !errors.Is(err, ErrActionRecordNotFound) {
		t.Fatalf("Advance(unknown) = %v", err)
	}
}

// ADR-0154 stage 2: with a durable store, a side-effecting call's success
// survives a "restart" (a fresh cache over the same store) and the retry is
// answered from the record; a failed call stays dispatched and may run again.
func TestDurableIdempotencyCache_SurvivesARestart(t *testing.T) {
	ctx := context.Background()
	store := NewMemActionRecordStore()
	meta := DurableAction{Key: IdempotencyKey("run-1", 0, "erp.create_order", []byte(`{"qty":3}`)),
		Tool: "erp.create_order", RunID: "run-1", StepIndex: 0, Principal: "ayse"}

	c1 := NewDurableIdempotencyCache(time.Hour, store)
	_, found, finish := c1.BeginAction(ctx, meta)
	if found {
		t.Fatal("first call found a stored result")
	}
	if rec, _ := store.Get(ctx, meta.Key); rec.State != ActionStateDispatched || rec.Principal != "ayse" {
		t.Fatalf("before the call ran the record is %+v; want dispatched", rec)
	}
	finish(ToolCallResponse{ResultJSON: []byte(`{"order":"SO-1"}`)}, true)
	if rec, _ := store.Get(ctx, meta.Key); rec.State != ActionStateAccepted {
		t.Fatalf("after success the record is %s; want accepted", rec.State)
	}

	c2 := NewDurableIdempotencyCache(time.Hour, store) // the kernel restarted
	stored, found, _ := c2.BeginAction(ctx, meta)
	if !found || string(stored.ResultJSON) != `{"order":"SO-1"}` {
		t.Fatalf("after a restart the retry ran again (found=%v, %s)", found, stored.ResultJSON)
	}

	failed := meta
	failed.Key = IdempotencyKey("run-1", 1, "erp.create_order", []byte(`{"qty":4}`))
	_, _, finish = c2.BeginAction(ctx, failed)
	finish(ToolCallResponse{Error: "upstream timeout"}, false)
	if rec, _ := store.Get(ctx, failed.Key); rec.State != ActionStateDispatched || rec.Error != "upstream timeout" {
		t.Fatalf("failed call record = %+v; want dispatched with the error", rec)
	}
	c3 := NewDurableIdempotencyCache(time.Hour, store)
	if _, found, finish := c3.BeginAction(ctx, failed); found {
		t.Fatal("a failed call was answered from the record; it must run again")
	} else {
		finish(ToolCallResponse{ResultJSON: []byte(`{}`)}, true)
	}
	if rec, _ := store.Get(ctx, failed.Key); rec.State != ActionStateAccepted || rec.Error != "" {
		t.Fatalf("re-dispatched call record = %+v; want accepted", rec)
	}
}

// Without a store the cache is the in-memory one (ADR-0154 D6).
func TestIdempotencyCache_NoStoreIsInMemoryOnly(t *testing.T) {
	ctx := context.Background()
	c := NewDurableIdempotencyCache(time.Hour, nil)
	_, _, finish := c.Begin(ctx, "k")
	finish(ToolCallResponse{ResultJSON: []byte(`1`)}, true)
	if _, found, _ := c.Begin(ctx, "k"); !found {
		t.Fatal("in-memory replay lost")
	}
	if _, found, _ := NewDurableIdempotencyCache(time.Hour, nil).Begin(ctx, "k"); found {
		t.Fatal("a fresh in-memory cache remembered a call")
	}
}

// ENG-1760: Lookup answers from memory or an accepted durable record without
// claiming the key; an unsettled record or an unknown key is not found.
func TestIdempotencyCache_LookupDoesNotClaim(t *testing.T) {
	ctx := context.Background()
	store := NewMemActionRecordStore()
	c := NewDurableIdempotencyCache(time.Hour, store)
	if _, found := c.Lookup(ctx, "k"); found {
		t.Fatal("unknown key found")
	}
	_, _, finish := c.BeginAction(ctx, DurableAction{Key: "k", Tool: "run_subtasks"})
	if _, found := c.Lookup(ctx, "k"); found {
		t.Fatal("an in-flight (dispatched) key must not be found")
	}
	finish(ToolCallResponse{ResultJSON: []byte(`"done"`)}, true)
	if r, found := c.Lookup(ctx, "k"); !found || string(r.ResultJSON) != `"done"` {
		t.Fatalf("in-memory lookup = %v %s", found, r.ResultJSON)
	}
	fresh := NewDurableIdempotencyCache(time.Hour, store) // restarted
	if r, found := fresh.Lookup(ctx, "k"); !found || string(r.ResultJSON) != `"done"` {
		t.Fatalf("durable lookup after a restart = %v %s", found, r.ResultJSON)
	}
	// Lookup claimed nothing: a new key can still be begun and finished.
	if _, found, fin := fresh.BeginAction(ctx, DurableAction{Key: "k2"}); found {
		t.Fatal("k2 found")
	} else {
		fin(ToolCallResponse{Error: "x"}, false)
	}
	if _, found := fresh.Lookup(ctx, "k2"); found {
		t.Fatal("a failed call must not be found")
	}
	if _, found := NewIdempotencyCache(time.Hour).Lookup(ctx, "k"); found {
		t.Fatal("a fresh in-memory cache found a key")
	}
	var nilCache *IdempotencyCache
	if _, found := nilCache.Lookup(ctx, "k"); found {
		t.Fatal("nil cache found a key")
	}
	if SubtaskResultKey("sub-a") == SubtaskResultKey("sub-b") || SubtaskResultKey("sub-a") != SubtaskResultKey("sub-a") {
		t.Fatal("SubtaskResultKey must be deterministic and distinct per subtask")
	}
}

// metaHandler records the _meta each call carried.
type metaHandler struct{ metas []map[string]any }

func (h *metaHandler) Execute(_ context.Context, c ToolCall) ([]byte, error) {
	h.metas = append(h.metas, c.Meta)
	return []byte(`{"ok":true}`), nil
}

// ADR-0154 D4 (ENG-812): a side-effecting plan-step call carries its
// deterministic action key in the MCP request's _meta; a read, or a call outside
// a plan step, carries none. The key is the ADR-0145 D9 IdempotencyKey.
func TestExecutor_SideEffectingStepCallSendsItsActionKey(t *testing.T) {
	h := &metaHandler{}
	e := guardedExec(t, h)
	e.idempotency = nil // the key travels whether or not idempotent_writes is on
	args := []byte(`{"to":["a@x"],"subject":"Reminder"}`)
	e.Execute(stepCtx(StepGuard{}), ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: args})
	e.Execute(stepCtx(StepGuard{}), ToolCallRequest{AgentID: "w", ToolName: "crm_search", ArgsJSON: []byte(`{}`)})
	e.Execute(context.Background(), ToolCallRequest{AgentID: "w", ToolName: "mail_draft", ArgsJSON: args})
	if len(h.metas) != 3 {
		t.Fatalf("calls = %d", len(h.metas))
	}
	if got := h.metas[0][ActionKeyMetaKey]; got != IdempotencyKey("run-1", 2, "mail_draft", args) {
		t.Fatalf("write _meta key = %v", got)
	}
	if _, ok := h.metas[1][ActionKeyMetaKey]; ok {
		t.Fatal("a read carried an action key")
	}
	if _, ok := h.metas[2][ActionKeyMetaKey]; ok {
		t.Fatal("a call outside a plan step carried an action key")
	}
}
