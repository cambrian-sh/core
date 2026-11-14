package domain

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// T-20261107-approver-is-not-told-that-an-identical-side: an approval for a
// side-effecting call names an earlier call of the same conversation it may
// duplicate. These tests reproduce the two usecases incidents (an issue re-created
// with a different summary; a mail re-sent with a near-identical body) and pin the
// deterministic matching rules.

func norm(s string) actionArgs { return normaliseActionArgs([]byte(s)) }

func TestMatchActionArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, prior, cand string
		want              bool
		how               string
	}{
		{"identical modulo case, whitespace, key and recipient order",
			`{"to":["Bob@x.com","alice@x.com"],"subject":"Q3  report","body":"Hi"}`,
			`{"body":"hi","subject":"q3 report","to":["alice@x.com","bob@x.com"]}`, true, "identical arguments"},
		{"empty values do not make calls differ",
			`{"to":"a@x.com","subject":"Hi","cc":""}`, `{"to":"a@x.com","subject":"Hi"}`, true, "identical arguments"},
		// usecases o2 #1: kernel-U2 lines 417 vs 594.
		{"issue re-created with a reworded summary, same project",
			`{"project_key":"OPS","issue_type":"Incident","summary":"INC-1040 (Ashvale) - Database outage","description":"Primary DB down since 09:12"}`,
			`{"project_key":"OPS","issue_type":"Incident","summary":"INC-1040 - Database outage","description":"DB outage, primary down"}`,
			true, "same project_key, subject"},
		// usecases o3 #2: near-identical mail bodies.
		{"mail re-sent to the same recipient, same subject, reworded body",
			`{"to":"ops@ashvale.example","subject":"Incident INC-1040 update","body":"The database is back up."}`,
			`{"to":"ops@ashvale.example","subject":"Incident INC-1040 update","body":"Database restored; monitoring."}`,
			true, "same subject, to"},
		{"same recipient, no subject, near-identical body",
			`{"channel":"#ops","text":"Deploy of v2.3 finished, all green, rollback window open until 18:00"}`,
			`{"channel":"#ops","text":"deploy of v2.3 finished, all green, rollback window open until 18:00 today"}`,
			true, "same channel, content"},
		{"different recipient is a different write",
			`{"to":"a@x.com","subject":"Reminder","body":"x"}`, `{"to":"b@x.com","subject":"Reminder","body":"x"}`, false, ""},
		{"same recipient, different subject",
			`{"to":"a@x.com","subject":"Invoice 17","body":"x"}`, `{"to":"a@x.com","subject":"Lunch on Friday","body":"x"}`, false, ""},
		{"different project, same summary",
			`{"project_key":"OPS","summary":"Rotate keys"}`, `{"project_key":"SEC","summary":"Rotate keys"}`, false, ""},
		{"same file, different content",
			`{"path":"notes.md","content":"alpha beta gamma"}`, `{"path":"notes.md","content":"completely other text here"}`, false, ""},
		{"nothing shared to compare",
			`{"amount":5,"currency":"EUR"}`, `{"amount":7,"currency":"EUR"}`, false, ""},
		{"one-word subject inside another is not enough",
			`{"to":"a@x.com","subject":"Hello"}`, `{"to":"a@x.com","subject":"Hello from the quarterly planning group"}`, false, ""},
		// T-20261108-duplicate-notice-misses-a-repeated-record-write: kernel-U8
		// lines 635 vs 676 (g-en-records-request-1 #1), same ref and lists, the
		// identical letter, 'bases' reworded.
		{"same record decided twice, a free-text note reworded",
			`{"ref":"FOI-2026-0001","letter":"Dear George Morris, ...","release":["HDC/EW/2026/00110","HDC/EW/2026/00111"],"withhold":["HDC/LS/2026/00114"],"bases":"00114 withheld under s.42"}`,
			`{"ref":"FOI-2026-0001","letter":"Dear George Morris, ...","release":["HDC/EW/2026/00111","HDC/EW/2026/00110"],"withhold":["HDC/LS/2026/00114"],"bases":"HDC/LS/2026/00114 withheld under FOIA 2000 s.42. 00112 redacted under s.40(2)."}`,
			true, "same content, ref"},
		{"same record decision, letter reworded too",
			`{"ref":"FOI-2026-0001","letter":"Dear Mr Morris, here is our response.","release":["A","B"],"withhold":["C"]}`,
			`{"ref":"FOI-2026-0001","letter":"Completely different wording of the reply to the requester.","release":["B","A"],"withhold":["C"]}`,
			true, "same ref, release, withhold"},
		{"same record, a different decision",
			`{"ref":"FOI-2026-0001","release":["A","B"],"withhold":["C"]}`,
			`{"ref":"FOI-2026-0001","release":["A"],"withhold":["B","C"]}`, false, ""},
		{"same record, a list on one side only",
			`{"ref":"FOI-2026-0001","release":["A","B"]}`,
			`{"ref":"FOI-2026-0001","release":["A","B"],"withhold":["C"]}`, false, ""},
		{"different records, same decision",
			`{"ref":"FOI-2026-0001","release":["A"]}`, `{"ref":"FOI-2026-0002","release":["A"]}`, false, ""},
		{"same record, different note and no structured decision",
			`{"case_id":"C-7","note":"called the resident, no answer"}`,
			`{"case_id":"C-7","note":"resident confirmed the repair is done"}`, false, ""},
		{"lists without a record key are not a decision on a record",
			`{"labels":["a","b"],"amount":5}`, `{"labels":["a","b"],"amount":7}`, false, ""},
		{"nested fields compare by leaf name",
			`{"fields":{"project":{"key":"OPS"},"summary":"Disk full on db-2"}}`,
			`{"fields":{"project":{"key":"OPS"},"summary":"Disk full on db-2 (urgent)"}}`, true, "same subject"},
	}
	for _, c := range cases {
		how, got := matchActionArgs(norm(c.prior), norm(c.cand))
		if got != c.want {
			t.Errorf("%s: match = %v (%q), want %v", c.name, got, how, c.want)
			continue
		}
		if c.want && how != c.how {
			t.Errorf("%s: how = %q, want %q", c.name, how, c.how)
		}
	}
}

func TestActionHistory_ScopeWindowAndStates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	h := NewActionHistory(time.Hour, clock)
	args := []byte(`{"to":"a@x.com","subject":"Hi"}`)

	ref := h.Begin("conversation:c1", "send_mail", args, ActionAwaitingApproval)
	h.Approved(ref, "apr-1")
	h.Finish(ref, []string{"message_id=m-1"})

	if got := h.Matches("conversation:c2", "send_mail", args, ActionRef{}); len(got) != 0 {
		t.Fatalf("another conversation must not match: %+v", got)
	}
	if got := h.Matches("conversation:c1", "post_message", args, ActionRef{}); len(got) != 0 {
		t.Fatalf("another tool must not match: %+v", got)
	}
	got := h.Matches("conversation:c1", "send_mail", args, ActionRef{})
	if len(got) != 1 || got[0].Prior.ApprovalID != "apr-1" || got[0].Prior.State != ActionDone {
		t.Fatalf("matches = %+v", got)
	}
	notice := RenderDuplicateNotice(got)
	for _, want := range []string{"Possible duplicate of send_mail", "approval apr-1", "done at 2026-11-07 17:00:00 UTC",
		"ids: message_id=m-1", "identical arguments"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q lacks %q", notice, want)
		}
	}

	// A refused call is dropped: it never ran.
	r2 := h.Begin("conversation:c1", "delete_repo", []byte(`{"repo":"x"}`), ActionAwaitingApproval)
	h.Drop(r2)
	if got := h.Matches("conversation:c1", "delete_repo", []byte(`{"repo":"x"}`), ActionRef{}); len(got) != 0 {
		t.Fatalf("a dropped call must not match: %+v", got)
	}

	// Past the window the done call no longer matches; a call still awaiting its
	// approval never expires.
	pending := h.Begin("conversation:c1", "send_mail", []byte(`{"to":"z@x.com","subject":"Later"}`), ActionAwaitingApproval)
	now = now.Add(2 * time.Hour)
	if got := h.Matches("conversation:c1", "send_mail", args, ActionRef{}); len(got) != 0 {
		t.Fatalf("an expired call must not match: %+v", got)
	}
	got = h.Matches("conversation:c1", "send_mail", []byte(`{"to":"z@x.com","subject":"Later"}`), ActionRef{})
	if len(got) != 1 || got[0].Prior.State != ActionAwaitingApproval {
		t.Fatalf("pending call must still match: %+v", got)
	}
	// Its own entry is excluded.
	if got := h.Matches("conversation:c1", "send_mail", []byte(`{"to":"z@x.com","subject":"Later"}`), pending); len(got) != 0 {
		t.Fatalf("a call must not match itself: %+v", got)
	}

	// Disabled: nil history, every method a no-op.
	var off *ActionHistory = NewActionHistory(0, nil)
	if off != nil {
		t.Fatal("window 0 must disable the history")
	}
	off.Finish(off.Begin("conversation:c1", "x", nil, ActionRunning), nil)
	if off.Matches("conversation:c1", "x", nil, ActionRef{}) != nil {
		t.Fatal("disabled history matched")
	}
}

func TestActionHistory_Bounded(t *testing.T) {
	t.Parallel()
	h := NewActionHistory(time.Hour, nil)
	for i := 0; i < actionHistoryPerScope+50; i++ {
		h.Finish(h.Begin("conversation:c", "t", []byte(fmt.Sprintf(`{"n":%d}`, i)), ActionRunning), nil)
	}
	h.mu.Lock()
	n := len(h.scopes["conversation:c"])
	h.mu.Unlock()
	if n != actionHistoryPerScope {
		t.Fatalf("per-scope entries = %d, want %d", n, actionHistoryPerScope)
	}
	for i := 0; i < actionHistoryMaxScopes+10; i++ {
		h.Begin(fmt.Sprintf("conversation:%d", i), "t", nil, ActionRunning)
	}
	h.mu.Lock()
	s := len(h.scopes)
	h.mu.Unlock()
	if s > actionHistoryMaxScopes {
		t.Fatalf("scopes = %d, want <= %d", s, actionHistoryMaxScopes)
	}
}

// recordingApproval answers every request and records what the approver was shown.
type recordingApproval struct {
	mu      sync.Mutex
	approve bool
	reqs    []ApprovalRequest
}

func (r *recordingApproval) Request(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	id := fmt.Sprintf("apr-%d", len(r.reqs))
	if !r.approve {
		return ApprovalDecision{State: ApprovalDenied, Decided: true, ApprovalID: id}, nil
	}
	return ApprovalDecision{Approved: true, Decided: true, State: ApprovalApproved, ApproverID: "op", ApprovalID: id}, nil
}

func (r *recordingApproval) notices() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.reqs))
	for i, q := range r.reqs {
		out[i] = q.DuplicateNotice
	}
	return out
}

func dupExec(appr ApprovalController, h *fakeHandler) *ToolExecutor {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "create_issue", Dangerous: true})
	reg.Register(SystemTool{Name: "post_note", Effects: []ToolEffect{EffectWrite}})
	reg.Register(SystemTool{Name: "search", Effects: []ToolEffect{EffectRead, EffectEgress}})
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "create_issue"}, ToolGrant{Tool: "post_note"}, ToolGrant{Tool: "search"}), h)
	e.Approval = appr
	e.History = NewActionHistory(24*time.Hour, nil)
	return e
}

// The incident: a plan creates the issue; the chat agent re-delegates and a new
// run asks to create it again with a reworded summary. The second approval must
// tell the approver about the first, with its approval id and the key it created.
func TestExecute_ApprovalNamesEarlierCallOfTheConversation(t *testing.T) {
	t.Parallel()
	appr := &recordingApproval{approve: true}
	h := &fakeHandler{result: []byte(`{"key":"OPS-11","self":"https://jira/OPS-11"}`)}
	e := dupExec(appr, h)
	ctx := WithConversationID(context.Background(), "conv-1")

	first := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue",
		ArgsJSON: []byte(`{"project_key":"OPS","summary":"INC-1040 (Ashvale) - Database outage"}`)})
	if first.Denied || first.Error != "" {
		t.Fatalf("first call: %+v", first)
	}
	second := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue",
		ArgsJSON: []byte(`{"project_key":"OPS","summary":"INC-1040 - Database outage"}`)})
	if second.Denied || second.Error != "" {
		t.Fatalf("the notice must never refuse the call: %+v", second)
	}
	n := appr.notices()
	if len(n) != 2 || n[0] != "" {
		t.Fatalf("notices = %q; the first call has nothing to duplicate", n)
	}
	for _, want := range []string{"Possible duplicate of create_issue", "approval apr-1", "done at", "ids: key=OPS-11",
		"same project_key, subject"} {
		if !strings.Contains(n[1], want) {
			t.Errorf("second notice %q lacks %q", n[1], want)
		}
	}

	// Another conversation knows nothing of it.
	other := WithConversationID(context.Background(), "conv-2")
	e.Execute(other, ToolCallRequest{AgentID: "a", ToolName: "create_issue",
		ArgsJSON: []byte(`{"project_key":"OPS","summary":"INC-1040 - Database outage"}`)})
	if n := appr.notices(); n[2] != "" {
		t.Fatalf("a call in another conversation got a notice: %q", n[2])
	}
}

// T-20261108-duplicate-notice-misses-a-repeated-record-write, the executor path:
// one chat-delegated plan, step 0 (asked to draft) and step 1 (asked to record)
// each call the record write, each under its own step session but the same
// ordering conversation. The second approval names the first.
func TestExecute_RepeatedRecordWriteInOnePlanIsFlagged(t *testing.T) {
	t.Parallel()
	appr := &recordingApproval{approve: true}
	h := &fakeHandler{result: []byte(`{"ok":true,"ref":"FOI-2026-0001"}`)}
	reg := NewInMemoryToolRegistry()
	const tool = "mcp:hdc-per-005/cases_respond_request"
	reg.Register(SystemTool{Name: tool, Dangerous: true, Effects: []ToolEffect{EffectWrite}})
	e := newExec(reg, grantStore("assistant", ToolGrant{Tool: tool}), h)
	e.Approval = appr
	e.History = NewActionHistory(24*time.Hour, nil)

	step := func(session, bases string) ToolCallResponse {
		ctx := WithSessionID(WithConversationID(context.Background(), "conv-foi"), SessionID(session))
		return e.Execute(ctx, ToolCallRequest{AgentID: "assistant", ToolName: tool, ArgsJSON: []byte(`{"ref":"FOI-2026-0001",` +
			`"letter":"FOI-2026-0001 Dear George Morris, thank you for your request.","release":["HDC/EW/2026/00110","HDC/EW/2026/00111"],` +
			`"release_redacted":["HDC/CS/2026/00112","HDC/EN/2026/00113"],"withhold":["HDC/LS/2026/00114","HDC/HR/2026/00189"],` +
			`"refer_published":["HDC/DS/2025/00109"],"bases":"` + bases + `"}`)})
	}
	if r := step("plan-run-1-step-0", "HDC/LS/2026/00114 withheld under FOIA 2000 s.42 (legal professional privilege)"); r.Denied {
		t.Fatalf("first: %+v", r)
	}
	if r := step("plan-run-1-step-1", "HDC/LS/2026/00114 withheld under FOIA 2000 s.42. HDC/DS/2025/00109 referred under s.21."); r.Denied {
		t.Fatalf("the notice must never refuse: %+v", r)
	}
	n := appr.notices()
	if len(n) != 2 || n[0] != "" {
		t.Fatalf("notices = %q", n)
	}
	for _, want := range []string{"Possible duplicate of " + tool, "approval apr-1", "done at", "ref"} {
		if !strings.Contains(n[1], want) {
			t.Errorf("second notice %q lacks %q", n[1], want)
		}
	}
}

// A write that needed no approval still counts (a later approved call may repeat
// it); a read-only call does not; a refused call never ran and does not count.
func TestExecute_HistoryRecordsOnlyWritesThatRan(t *testing.T) {
	t.Parallel()
	appr := &recordingApproval{approve: false}
	h := &fakeHandler{result: []byte(`{"id":"n-1"}`)}
	e := dupExec(appr, h)
	ctx := WithConversationID(context.Background(), "conv-1")
	args := []byte(`{"project_key":"OPS","summary":"Rotate the DB credentials"}`)

	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue", ArgsJSON: args}) // denied
	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue", ArgsJSON: args}) // denied again
	if n := appr.notices(); n[1] != "" {
		t.Fatalf("a refused call must not be reported as done: %q", n[1])
	}

	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "search", ArgsJSON: args})
	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "post_note", ArgsJSON: args})
	got := e.History.Matches(ActionHistoryScope("conv-1", ""), "post_note", args, ActionRef{})
	if len(got) != 1 || got[0].Prior.State != ActionDone || got[0].Prior.ApprovalID != "" ||
		strings.Join(got[0].Prior.IDs, ",") != "id=n-1" {
		t.Fatalf("post_note history = %+v", got)
	}
	if got := e.History.Matches(ActionHistoryScope("conv-1", ""), "search", args, ActionRef{}); len(got) != 0 {
		t.Fatalf("a read-only call was recorded: %+v", got)
	}

	// A failed write is dropped: only successful writes count (owner decision
	// 2026-11-10, T-20261109-duplicate-notice-fires-on-the-retry-of-a-write).
	h.err = fmt.Errorf("upstream 502")
	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "post_note", ArgsJSON: []byte(`{"path":"x.md","content":"q"}`)})
	if got := e.History.Matches(ActionHistoryScope("conv-1", ""), "post_note", []byte(`{"path":"x.md","content":"q"}`), ActionRef{}); len(got) != 0 {
		t.Fatalf("a failed write must not be kept: %+v", got)
	}
	// So is one whose result reports an error or a refusal in-band.
	h.err = nil
	for _, res := range []string{`{"error":"not_found"}`, `{"denied":"policy"}`} {
		h.result = []byte(res)
		e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "post_note", ArgsJSON: []byte(`{"path":"y.md","content":"r"}`)})
		if got := e.History.Matches(ActionHistoryScope("conv-1", ""), "post_note", []byte(`{"path":"y.md","content":"r"}`), ActionRef{}); len(got) != 0 {
			t.Fatalf("result %s must not be kept: %+v", res, got)
		}
	}
}

// T-20261109-duplicate-notice-fires-on-the-retry-of-a-write, the personal run
// incident (p07-calendar-free-slot #1): the first calendar_create_event named the
// attendees by name and the system answered not_found (an MCP isError result,
// which the MCP handler returns as the call's error); the corrected retry with the
// attendee ids must reach the approver WITHOUT a duplicate notice. A write that
// succeeded and is then repeated identically is still flagged.
func TestExecute_RetryAfterFailedWriteIsNotFlagged(t *testing.T) {
	t.Parallel()
	appr := &recordingApproval{approve: true}
	h := &fakeHandler{err: fmt.Errorf(`mcp tool "calendar_create_event" reported an error: {"error":"not_found","message":"No user Fenella Oakhurst in the directory."}`)}
	reg := NewInMemoryToolRegistry()
	const tool = "mcp:vantis-emp-001/calendar_create_event"
	reg.Register(SystemTool{Name: tool, Dangerous: true, Effects: []ToolEffect{EffectWrite}})
	e := newExec(reg, grantStore("composed_worker", ToolGrant{Tool: tool}), h)
	e.Approval = appr
	e.History = NewActionHistory(24*time.Hour, nil)
	ctx := WithConversationID(context.Background(), "conv-p07")
	call := func(attendees string) ToolCallResponse {
		return e.Execute(ctx, ToolCallRequest{AgentID: "composed_worker", ToolName: tool, ArgsJSON: []byte(
			`{"title":"Sync on Q4 priorities","start":"2026-11-10T10:30:00Z","end":"2026-11-10T11:00:00Z",` +
				`"attendees":[` + attendees + `],"description":"Sync on Q4 priorities"}`)})
	}

	if r := call(`"Fenella Oakhurst","Tobin Lanthorne"`); r.Error == "" {
		t.Fatalf("first call should fail: %+v", r)
	}
	h.err, h.result = nil, []byte(`{"event_id":"EV-1"}`)
	if r := call(`"EMP-006","EMP-011"`); r.Error != "" || r.Denied {
		t.Fatalf("corrected retry: %+v", r)
	}
	if r := call(`"EMP-006","EMP-011"`); r.Error != "" || r.Denied {
		t.Fatalf("identical repeat: %+v", r)
	}
	n := appr.notices()
	if len(n) != 3 {
		t.Fatalf("approvals = %d, want 3", len(n))
	}
	if n[1] != "" {
		t.Fatalf("the corrected retry of a failed write was flagged: %q", n[1])
	}
	for _, want := range []string{"Possible duplicate of " + tool, "approval apr-2", "done at", "event_id=EV-1"} {
		if !strings.Contains(n[2], want) {
			t.Errorf("repeat of a successful write: notice %q lacks %q", n[2], want)
		}
	}
}

// Two steps of one plan asking to send the same mail at the same time: the second
// approval names the first, still awaiting its own approval.
func TestExecute_ConcurrentDuplicateWhileFirstAwaitsApproval(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	first := make(chan struct{})
	appr := &blockingApproval{release: release, first: first}
	e := dupExec(appr, &fakeHandler{result: []byte(`{}`)})
	ctx := WithConversationID(context.Background(), "conv-1")
	args := []byte(`{"to":"ops@x.com","subject":"Outage update"}`)

	done := make(chan struct{})
	go func() {
		e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue", ArgsJSON: args})
		close(done)
	}()
	<-first
	notice := RenderDuplicateNotice(e.History.Matches(ActionHistoryScope("conv-1", ""), "create_issue", args, ActionRef{}))
	if !strings.Contains(notice, "awaiting approval") {
		t.Fatalf("notice = %q, want the first call shown awaiting approval", notice)
	}
	close(release)
	<-done
}

type blockingApproval struct {
	release, first chan struct{}
	once           sync.Once
}

func (b *blockingApproval) Request(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	b.once.Do(func() { close(b.first) })
	<-b.release
	return ApprovalDecision{Approved: true, Decided: true, State: ApprovalApproved, ApprovalID: "apr-1"}, nil
}

// noticeDroppingStore is a store without a duplicate_notice column (the Postgres
// adapter today).
type noticeDroppingStore struct{ ApprovalStore }

func (s noticeDroppingStore) PutApproval(ctx context.Context, it ApprovalItem) error {
	it.DuplicateNotice = ""
	return s.ApprovalStore.PutApproval(ctx, it)
}

func (s noticeDroppingStore) ResolveApproval(ctx context.Context, it ApprovalItem) (bool, error) {
	it.DuplicateNotice = ""
	return s.ApprovalStore.ResolveApproval(ctx, it)
}

// End to end through the real hub: the approver's stream and a later listing both
// carry the notice, even when the store does not persist it.
func TestHub_DuplicateNoticeReachesTheApprover(t *testing.T) {
	t.Parallel()
	hub := NewAddressedHub(AddressedHubConfig{Store: noticeDroppingStore{NewMemoryApprovalStore()}})
	evs, cancel := hub.Watch(opWatch("op"))
	defer cancel()
	e := dupExec(hub, &fakeHandler{result: []byte(`{"key":"OPS-11"}`)})
	ctx := WithConversationID(context.Background(), "conv-1")

	call := func(summary string) <-chan ToolCallResponse {
		out := make(chan ToolCallResponse, 1)
		go func() {
			out <- e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "create_issue",
				ArgsJSON: []byte(`{"project_key":"OPS","summary":"` + summary + `"}`)})
		}()
		return out
	}
	approve := func(it ApprovalItem) {
		if _, err := hub.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")}); err != nil {
			t.Fatal(err)
		}
	}

	r1 := call("INC-1040 (Ashvale) - Database outage")
	it1 := nextRaised(t, evs)
	if it1.DuplicateNotice != "" {
		t.Fatalf("first item has a notice: %q", it1.DuplicateNotice)
	}
	approve(it1)
	if r := <-r1; r.Denied {
		t.Fatalf("first call: %+v", r)
	}

	r2 := call("INC-1040 - Database outage")
	it2 := nextRaised(t, evs)
	if !strings.Contains(it2.DuplicateNotice, "approval "+it1.ID) || !strings.Contains(it2.DuplicateNotice, "key=OPS-11") {
		t.Fatalf("streamed item notice = %q", it2.DuplicateNotice)
	}
	listed, err := hub.List(context.Background(), ApprovalQuery{States: []ApprovalState{ApprovalPending}})
	if err != nil || len(listed) != 1 || listed[0].DuplicateNotice != it2.DuplicateNotice {
		t.Fatalf("listed = %+v, %v; want the notice restored", listed, err)
	}
	got, err := hub.Get(context.Background(), it2.ID)
	if err != nil || got.DuplicateNotice != it2.DuplicateNotice {
		t.Fatalf("get = %+v, %v", got, err)
	}
	approve(it2)
	if r := <-r2; r.Denied {
		t.Fatalf("an approved duplicate must run: %+v", r)
	}
	for {
		res := nextResolved(t, evs)
		if res.ID != it2.ID {
			continue // it1's resolution
		}
		if res.DuplicateNotice != it2.DuplicateNotice {
			t.Fatalf("resolution of the flagged item lost its notice: %q", res.DuplicateNotice)
		}
		break
	}
	hub.mu.Lock()
	left := len(hub.dupNotices)
	hub.mu.Unlock()
	if left != 0 {
		t.Fatalf("resolved items' notices still held in memory: %d", left)
	}
}

// preview used to cut at a byte offset: a multi-byte rune across the cut left
// invalid UTF-8 in ArgsPreview, which protobuf will not marshal and Postgres will
// not store.
func TestPreview_CutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	in := []byte(`{"subject":"` + strings.Repeat("ş", 150) + `"}`)
	for n := 1; n < len(in)+2; n++ {
		p := preview(in, n)
		if !utf8.ValidString(p) || len(p) > n {
			t.Fatalf("preview(%d) = %q: valid=%v len=%d", n, p, utf8.ValidString(p), len(p))
		}
	}
	if got := preview([]byte("abc"), 10); got != "abc" {
		t.Fatalf("short input changed: %q", got)
	}
}

// T-20261113-systemtool-sideeffecting-ignores-egress-a-non: egress_send is a send
// (ADR-0150 D9) and counts as side-effecting even on a non-dangerous tool, so the
// idempotency cache, the action history and the plan ledger see it. egress_read
// and plain (legacy) egress on a non-dangerous tool do not — the shipped catalog
// declares [read, egress] on pure reads.
func TestSystemTool_SideEffecting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool SystemTool
		want bool
	}{
		{"read only", SystemTool{Effects: []ToolEffect{EffectRead}}, false},
		{"egress_read", SystemTool{Effects: []ToolEffect{EffectRead, EffectEgressRead}}, false},
		{"plain egress read (catalog shape)", SystemTool{Effects: []ToolEffect{EffectRead, EffectEgress}}, false},
		{"egress_send, not dangerous", SystemTool{Effects: []ToolEffect{EffectEgressSend}}, true},
		{"egress_read + egress_send", SystemTool{Effects: []ToolEffect{EffectEgressRead, EffectEgressSend}}, true},
		{"write", SystemTool{Effects: []ToolEffect{EffectWrite}}, true},
		{"spend", SystemTool{Effects: []ToolEffect{EffectSpend}}, true},
		{"admin", SystemTool{Effects: []ToolEffect{EffectAdmin}}, true},
		{"dangerous read", SystemTool{Dangerous: true, Effects: []ToolEffect{EffectRead}}, true},
	}
	for _, c := range cases {
		if got := c.tool.SideEffecting(); got != c.want {
			t.Errorf("%s: SideEffecting() = %v, want %v", c.name, got, c.want)
		}
	}
}
