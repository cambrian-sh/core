package domain

import (
	"context"
	"testing"
)

// ENG-1428 stage (a): the ADR-0150 subtask idempotency cache is the ADR-0154 durable
// action record. A subtask resumed after a KERNEL RESTART (a new registry and a new
// executor over the same store) is answered from the record — the write runs once —
// and its side-effecting calls carry their deterministic key in the MCP _meta
// (ENG-812), which subtask calls did not before.
func TestSubtaskWrite_SurvivesARestartThroughTheActionRecord(t *testing.T) {
	store := NewMemActionRecordStore()
	widened := []ToolEffect{EffectRead, EffectWrite}
	args := []byte(`{"id":"CRM-9","stage":"won"}`)

	boot := func(h ToolHandler) (*ToolExecutor, *Subtasks) {
		e, _ := subtaskExec(t, h)
		sub := NewSubtasks(0, store)
		e.Subtasks, e.LeaseCeiling = sub, sub
		e.Approval = &labelApproval{}
		return e, sub
	}

	h1 := &metaHandler{}
	e1, sub1 := boot(h1)
	bindChild(sub1, "child-a", "sub-same", widened, "crm_update")
	first := e1.Execute(context.Background(), ToolCallRequest{AgentID: "composed_worker", ToolName: "crm_update",
		ArgsJSON: args, SessionTokenID: "child-a"})
	if first.Denied || first.Error != "" || len(h1.metas) != 1 {
		t.Fatalf("first write must run once: %+v (calls %d)", first, len(h1.metas))
	}
	key, _ := h1.metas[0][ActionKeyMetaKey].(string)
	if key == "" {
		t.Fatal("a subtask's side-effecting call must carry its action key in _meta")
	}
	rec, err := store.Get(context.Background(), key)
	if err != nil || rec.State != ActionStateAccepted || rec.Tool != "crm_update" {
		t.Fatalf("the write must be recorded accepted under its key: %+v, %v", rec, err)
	}

	// Restart: everything in memory is gone; the store is not.
	h2 := &metaHandler{}
	e2, sub2 := boot(h2)
	bindChild(sub2, "child-b", "sub-same", widened, "crm_update")
	again := e2.Execute(context.Background(), ToolCallRequest{AgentID: "composed_worker", ToolName: "crm_update",
		ArgsJSON: []byte(`{"stage":"won","id":"CRM-9"}`), SessionTokenID: "child-b"})
	if len(h2.metas) != 0 {
		t.Fatalf("the resumed subtask repeated the write after a restart: %d calls", len(h2.metas))
	}
	if string(again.ResultJSON) != string(first.ResultJSON) {
		t.Fatalf("the resumed call must be answered from the record: %+v", again)
	}
}
