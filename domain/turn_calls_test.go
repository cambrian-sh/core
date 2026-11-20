package domain

import (
	"context"
	"testing"
)

// ENG-1634: the record of what a chat turn's calls began, and its turn-scoped keys.

func TestTurnCalls_OnlyTrackedLeasesAreRecordedAndKeysAreTurnScoped(t *testing.T) {
	tc := NewTurnCalls(nil)
	if _, ok := tc.Begin("lease-x", "mail_send", []byte(`{}`)); ok {
		t.Fatal("an untracked lease is not a chat turn's and must not be recorded")
	}
	tc.Track("lease-1")
	k1, ok := tc.Begin("lease-1", "mail_send", []byte(`{"to":"a","s":"b"}`))
	if !ok || len(k1) < 10 {
		t.Fatalf("key %q ok=%v", k1, ok)
	}
	// Same tool and canonical args (key order differs): the second occurrence, another key.
	k2, _ := tc.Begin("lease-1", "mail_send", []byte(`{"s":"b","to":"a"}`))
	if k2 == k1 {
		t.Fatal("the second identical call in one attempt is a new occurrence, a new key")
	}
	if n, tools := tc.Begun("lease-1"); n != 2 || len(tools) != 1 || tools[0] != "mail_send" {
		t.Fatalf("begun = %d %v", n, tools)
	}
	// Another lease never shares a key.
	tc.Track("lease-2")
	if k, _ := tc.Begin("lease-2", "mail_send", []byte(`{"to":"a","s":"b"}`)); k == k1 {
		t.Fatal("keys are turn-scoped: another turn's identical call must not share a key")
	}
	// A re-drive starts occurrences again: its first identical call maps onto the
	// killed attempt's first.
	tc.Redrive("lease-1")
	if k, _ := tc.Begin("lease-1", "mail_send", []byte(`{"to":"a","s":"b"}`)); k != k1 {
		t.Fatalf("after Redrive the first occurrence must map to the same key: %q vs %q", k, k1)
	}
	if n, _ := tc.Begun("lease-1"); n != 3 {
		t.Fatalf("begun after the re-drive = %d, want 3 (it keeps counting)", n)
	}
	tc.Forget("lease-1")
	if n, _ := tc.Begun("lease-1"); n != 0 {
		t.Fatal("a forgotten lease has no record")
	}
	if _, ok := tc.Begin("lease-1", "mail_send", nil); ok {
		t.Fatal("a forgotten lease is no longer recorded")
	}
	var nilTC *TurnCalls
	nilTC.Track("l")
	if _, ok := nilTC.Begin("l", "t", nil); ok {
		t.Fatal("a nil record records nothing")
	}
}

// Through the executor: a side-effecting call on a tracked turn lease is counted
// and keyed; a re-driven turn's identical call is answered from the stored result
// (the write runs once); reads, untracked leases and plan-step calls are not
// recorded as the turn's.
func TestTurnCalls_TheExecutorRecordsAndDeduplicatesATurnsWrites(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"MSG-9"}`)}
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "crm_search", Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: "mail_send", Effects: []ToolEffect{EffectWrite}})
	tc := NewTurnCalls(nil)
	e := &ToolExecutor{Registry: reg, Handler: h, InlineThreshold: 1 << 20, unrestricted: true, turnCalls: tc}
	ctx := context.Background()
	tc.Track("turn-lease")

	e.Execute(ctx, ToolCallRequest{AgentID: "chat", ToolName: "crm_search", SessionTokenID: "turn-lease", ArgsJSON: []byte(`{}`)})
	if n, _ := tc.Begun("turn-lease"); n != 0 {
		t.Fatalf("a read began a side effect: %d", n)
	}
	first := e.Execute(ctx, ToolCallRequest{AgentID: "chat", ToolName: "mail_send", SessionTokenID: "turn-lease", ArgsJSON: []byte(`{"to":"a"}`)})
	if first.Denied || first.Error != "" {
		t.Fatalf("send refused: %+v", first)
	}
	if n, tools := tc.Begun("turn-lease"); n != 1 || tools[0] != "mail_send" {
		t.Fatalf("begun = %d %v", n, tools)
	}
	tc.Redrive("turn-lease")
	again := e.Execute(ctx, ToolCallRequest{AgentID: "chat", ToolName: "mail_send", SessionTokenID: "turn-lease", ArgsJSON: []byte(`{"to":"a"}`)})
	if h.n.Load() != 2 || string(again.ResultJSON) != string(first.ResultJSON) {
		t.Fatalf("the re-driven identical write ran again (handler calls %d) or answered differently: %s vs %s",
			h.n.Load(), again.ResultJSON, first.ResultJSON)
	}
	// Not the turn's: an untracked lease, a plan step.
	e.Execute(ctx, ToolCallRequest{AgentID: "chat", ToolName: "mail_send", SessionTokenID: "other", ArgsJSON: []byte(`{"to":"a"}`)})
	e.Execute(WithStepCall(ctx, StepCall{RunID: "r", StepIndex: 1}),
		ToolCallRequest{AgentID: "w", ToolName: "mail_send", SessionTokenID: "turn-lease", ArgsJSON: []byte(`{"to":"b"}`)})
	if n, _ := tc.Begun("turn-lease"); n != 2 {
		t.Fatalf("begun = %d, want 2 (only the turn's own calls)", n)
	}
}
