package domain

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// ADR-0145 D14 request-tied guards, each reproducing a replay-gate failure offline
// (T-20261109-adr-0145-d14-fix-batch-remove-per-step).

// scriptedApprover answers approvals from a script keyed by the "to" argument and
// records every request it saw.
type scriptedApprover struct {
	mu   sync.Mutex
	deny map[string]bool
	seen []ApprovalRequest
}

func (a *scriptedApprover) Request(_ context.Context, r ApprovalRequest) (ApprovalDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, r)
	for k := range a.deny {
		if strings.Contains(string(r.Args), k) {
			return ApprovalDecision{Approved: false, Decided: true, State: ApprovalDenied, ApproverID: "person"}, nil
		}
	}
	return ApprovalDecision{Approved: true, Decided: true, State: ApprovalApproved, ApproverID: "person"}, nil
}

func (a *scriptedApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

func d14Exec(h ToolHandler, ap ApprovalController) *ToolExecutor {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "crm_search", Effects: []ToolEffect{EffectRead}})
	reg.Register(SystemTool{Name: "mail_draft", Dangerous: true, Effects: []ToolEffect{EffectWrite}})
	reg.Register(SystemTool{Name: "mail_send", Dangerous: true, Effects: []ToolEffect{EffectWrite, EffectEgress}})
	reg.Register(SystemTool{Name: "cases_respond_request", Effects: []ToolEffect{EffectWrite}})
	reg.Register(SystemTool{Name: "web_fetch", Effects: []ToolEffect{EffectRead, EffectEgress}})
	return &ToolExecutor{Registry: reg, Handler: h, InlineThreshold: 1 << 20, Unrestricted: true, Approval: ap,
		Guards: &RequestGuards{
			Targets: &WriteTargets{
				Rules:       []WriteTargetRule{{Tools: []string{"*mail*"}, Keys: []string{"to"}}},
				DefaultKeys: []string{"ref", "case_id"},
			},
			Writes:   NewWriteRecord(0),
			Refusals: NewRefusalRecord(0),
		}}
}

func guardStep(run RunID, step int, g StepGuard) context.Context {
	if g.Request == "" {
		g.Request = "turn:conv-1/msg-1"
	}
	return WithStepCall(context.Background(), StepCall{RunID: run, StepIndex: step, Guard: g})
}

func draft(to, body string) ToolCallRequest {
	return ToolCallRequest{AgentID: "w", ToolName: "mail_draft",
		ArgsJSON: []byte(`{"to":"` + to + `","subject":"Overdue invoice","body":"` + body + `"}`)}
}

// o3x #2: stage 1 saved two drafts (approved) and one more was refused; stage 2
// (a new run of the same request) planned the drafts again with re-worded text.
// The re-drafts are refused without asking anyone; a recipient never written is
// still allowed, and the refused one wrote nothing so it is not "taken".
func TestD14_StageTwoCannotRedraftTargetsStageOneWrote(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"W-000002"}`)}
	ap := &scriptedApprover{deny: map[string]bool{"carrow@": true}}
	e := d14Exec(h, ap)
	s1 := guardStep("run-a", 1, StepGuard{Checklist: true, WriteItems: []string{"R2"}})
	for _, c := range []ToolCallRequest{
		draft("brisa.ashcombe@wychwood.example", "Dear Brisa, invoice INV-1 is 75 days overdue."),
		draft("Ops <ops@marlowe.example>", "Hello, INV-2 is 62 days overdue."),
		draft("carrow@hollins.example", "INV-3 overdue."),
	} {
		e.Execute(s1, c)
	}
	if h.n.Load() != 2 {
		t.Fatalf("stage 1 should have run 2 approved drafts, ran %d", h.n.Load())
	}
	// Stage 2: another run id of the same request, step answers only an answer item.
	s2 := guardStep("run-a-s2", 0, StepGuard{Checklist: true, WriteItems: nil})
	asked := ap.count()
	for _, c := range []ToolCallRequest{
		draft("brisa.ashcombe@wychwood.example", "Hi Brisa — a gentle reminder about INV-1."),
		draft("ops@marlowe.example", "Reminder: INV-2."),
	} {
		got := e.Execute(s2, c)
		if !got.Denied || !strings.Contains(got.DenyReason, "already written in this request") {
			t.Fatalf("stage-2 re-draft not refused: %+v", got)
		}
		if !strings.Contains(got.DenyReason, "W-000002") {
			t.Fatalf("refusal should name the earlier result: %q", got.DenyReason)
		}
	}
	if h.n.Load() != 2 || ap.count() != asked {
		t.Fatalf("a refused duplicate must not run nor ask: ran %d, asked %d→%d", h.n.Load(), asked, ap.count())
	}
	// The refused recipient was never written: a write to it is not a duplicate.
	ap.deny = nil
	if got := e.Execute(guardStep("run-a-s2", 1, StepGuard{Checklist: true, WriteItems: []string{"R3"}}), draft("carrow@hollins.example", "INV-3.")); got.Denied {
		t.Fatalf("a target only refused earlier is still writable: %+v", got)
	}
	if ts := e.Guards.Writes.Targets("turn:conv-1/msg-1"); len(ts) != 3 {
		t.Fatalf("written targets = %+v", ts)
	}
}

// g-en-records-request-1: the D6 repair turn re-ran the write with a re-worded
// letter. In the repair turn a side-effecting call is refused once the step's
// write executed — even one with no recognisable target.
func TestD14_RepairTurnNeverRerunsAnExecutedWrite(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"W-000001"}`)}
	e := d14Exec(h, nil)
	first := ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-2026-0001","letter":"Dear applicant, ..."}`)}
	if got := e.Execute(guardStep("run-r", 2, StepGuard{Checklist: true, WriteItems: []string{"R2"}}), first); got.Denied || got.Error != "" {
		t.Fatalf("first write: %+v", got)
	}
	repair := guardStep("run-r", 2, StepGuard{Checklist: true, WriteItems: []string{"R2"}, Repair: true})
	reworded := ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-2026-0001","letter":"Dear Sir or Madam, ..."}`)}
	got := e.Execute(repair, reworded)
	if !got.Denied || !strings.Contains(got.DenyReason, "repair turn") || !strings.Contains(got.DenyReason, "W-000001") {
		t.Fatalf("repair turn re-ran the write: %+v", got)
	}
	// Another step of the same plan writing the same case is refused by the target record.
	got = e.Execute(guardStep("run-r", 3, StepGuard{Checklist: true, WriteItems: []string{"R2"}}), reworded)
	if !got.Denied || !strings.Contains(strings.ToLower(got.DenyReason), "foi-2026-0001") {
		t.Fatalf("second step re-wrote the case: %+v", got)
	}
	if h.n.Load() != 1 {
		t.Fatalf("the case was written %d times", h.n.Load())
	}
	// A read in the repair turn is fine.
	if got := e.Execute(repair, ToolCallRequest{AgentID: "w", ToolName: "crm_search", ArgsJSON: []byte(`{}`)}); got.Denied {
		t.Fatalf("a read in the repair turn: %+v", got)
	}
}

// forbids: send blocks every egress-class tool, with a reason that names the
// person's instruction; drafting (write only) stays allowed.
func TestD14_ForbidsSendBlocksEgressTools(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"X"}`)}
	e := d14Exec(h, &scriptedApprover{})
	cons := ResolveConstraints(&RequirementChecklist{Items: []RequirementItem{
		{ID: "R4", Type: RequirementConstraint, Quote: "do not send anything yet", Forbids: []string{"send"}},
	}}, map[string][]ToolEffect{"send": {EffectEgress}})
	ctx := guardStep("run-c", 0, StepGuard{Checklist: true, WriteItems: []string{"R1"}, Constraints: cons})
	for _, tool := range []string{"mail_send", "web_fetch"} {
		got := e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: tool, ArgsJSON: []byte(`{"to":"a@b.example"}`)})
		if !got.Denied || !strings.Contains(got.DenyReason, "do not send anything yet") || !strings.Contains(got.DenyReason, "egress") {
			t.Fatalf("%s not refused by forbids:send: %+v", tool, got)
		}
	}
	if got := e.Execute(ctx, draft("a@b.example", "draft only")); got.Denied {
		t.Fatalf("a draft is not a send: %+v", got)
	}
	if h.n.Load() != 1 {
		t.Fatalf("ran %d", h.n.Load())
	}
}

// A write on a step that answers no open write/document item goes to approval
// with the "not part of the request" notice — even for a tool that needs no
// approval on its own; without an approver it is refused saying so.
func TestD14_UnrequestedWriteGoesToApprovalWithNotice(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"W-9"}`)}
	ap := &scriptedApprover{}
	e := d14Exec(h, ap)
	ctx := guardStep("run-u", 0, StepGuard{Checklist: true}) // answers only an answer item
	call := ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-9"}`)}
	if got := e.Execute(ctx, call); got.Denied {
		t.Fatalf("approved unrequested write refused: %+v", got)
	}
	if ap.count() != 1 || !strings.HasPrefix(ap.seen[0].DuplicateNotice, UnrequestedWriteNoticePrefix) {
		t.Fatalf("approval notice = %+v", ap.seen)
	}
	// A requested write of the same non-dangerous tool asks nobody.
	e2 := d14Exec(h, ap)
	if got := e2.Execute(guardStep("run-u2", 0, StepGuard{Checklist: true, WriteItems: []string{"R2"}}),
		ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-10"}`)}); got.Denied || ap.count() != 1 {
		t.Fatalf("requested write: %+v asked %d", got, ap.count())
	}
	// No approver: refused, and the reason says why.
	e3 := d14Exec(h, nil)
	got := e3.Execute(guardStep("run-u3", 0, StepGuard{Checklist: true}), ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-11"}`)})
	if !got.Denied || !strings.Contains(got.DenyReason, UnrequestedWriteNoticePrefix) {
		t.Fatalf("unrequested write without approver: %+v", got)
	}
	// Without a checklist nothing is "unrequested".
	if got := e3.Execute(guardStep("run-u4", 0, StepGuard{}), ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request", ArgsJSON: []byte(`{"ref":"FOI-12"}`)}); got.Denied {
		t.Fatalf("no checklist: %+v", got)
	}
}

// T-20261109-workers-repeat-a-call-the-kernel-already: an identical refused call
// is answered from the step's record — no second approval, no run — and says so.
func TestD14_RepeatedRefusalAnsweredFromTheRecord(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"X"}`)}
	ap := &scriptedApprover{deny: map[string]bool{"blocked@": true}}
	e := d14Exec(h, ap)
	ctx := guardStep("run-x", 0, StepGuard{Checklist: true, WriteItems: []string{"R1"}})
	call := draft("blocked@x.example", "hello")
	first := e.Execute(ctx, call)
	second := e.Execute(ctx, ToolCallRequest{AgentID: "w", ToolName: "mail_draft",
		ArgsJSON: []byte(`{"body":"hello","subject":"Overdue invoice","to":"blocked@x.example"}`)}) // key order differs
	if !first.Denied || !second.Denied || ap.count() != 1 || h.n.Load() != 0 {
		t.Fatalf("repeat asked again or ran: first %+v second %+v asked %d ran %d", first, second, ap.count(), h.n.Load())
	}
	if !strings.Contains(second.DenyReason, "already refused in this step") || strings.Count(second.DenyReason, RepeatedRefusalNote) != 1 {
		t.Fatalf("repeat reason = %q", second.DenyReason)
	}
	if third := e.Execute(ctx, call); strings.Count(third.DenyReason, RepeatedRefusalNote) != 1 {
		t.Fatalf("note repeated: %q", third.DenyReason)
	}
	// Another step may ask (a different step is a different context).
	e.Execute(guardStep("run-x", 1, StepGuard{Checklist: true, WriteItems: []string{"R1"}}), call)
	if ap.count() != 2 {
		t.Fatalf("other step: asked %d", ap.count())
	}
}

func TestWriteTargets_KeyNormalisesRecipients(t *testing.T) {
	w := &WriteTargets{Rules: []WriteTargetRule{{Tools: []string{"*mail*"}, Keys: []string{"to"}}}, DefaultKeys: []string{"ref"}}
	a := w.Key("mcp:mail/mail_draft", []byte(`{"to":"Brisa Ashcombe <Brisa.Ashcombe@Wychwood.example>","body":"x"}`))
	b := w.Key("mail_draft", []byte(`{"to":["brisa.ashcombe@wychwood.example"],"body":"y"}`))
	if a == "" || a != b {
		t.Fatalf("keys differ: %q vs %q", a, b)
	}
	if k := w.Key("cases_respond_request", []byte(`{"ref":"FOI-1","letter":"x"}`)); k != "cases_respond_request|ref=foi-1" {
		t.Fatalf("record key = %q", k)
	}
	if k := w.Key("notes_append", []byte(`{"text":"x"}`)); k != "" {
		t.Fatalf("no target expected, got %q", k)
	}
}

// T-20261109-d14-lets-a-drafting-step-answers-only-the (records replay, run
// f278c4d74121d508): step 3 answered only R1 (document "Draft a response letter")
// and called cases_respond_request FOI-2026-0001; step 4 (answering R7, write
// "Record the response in the case system") then had its own write refused as a
// duplicate target. ADR-0145 D14.6: a document is finished inside Cambrian; the
// drafting step's outside write is refused with that reason, writes nothing, takes
// no target, and the planned write step goes through.
func TestD14_DraftingStepCannotWriteOutside_ThePlannedWriteStepCan(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"W-000001"}`)}
	ap := &scriptedApprover{}
	e := d14Exec(h, ap)
	write := ToolCallRequest{AgentID: "w", ToolName: "cases_respond_request",
		ArgsJSON: []byte(`{"ref":"FOI-2026-0001","release":[],"withhold":[]}`)}
	got := e.Execute(guardStep("run-d", 3, StepGuard{Checklist: true, Documents: []string{"R1"}}), write)
	if !got.Denied || !strings.Contains(got.DenyReason, "inside Cambrian") || !strings.Contains(got.DenyReason, "R1") {
		t.Fatalf("drafting step's outside write not refused with the reason: %+v", got)
	}
	if h.n.Load() != 0 || ap.count() != 0 {
		t.Fatalf("refused write ran (%d) or asked an approver (%d)", h.n.Load(), ap.count())
	}
	got = e.Execute(guardStep("run-d", 4, StepGuard{Checklist: true, WriteItems: []string{"R7"}}), write)
	if got.Denied || h.n.Load() != 1 {
		t.Fatalf("the planned write step was refused: %+v (ran %d)", got, h.n.Load())
	}
	// A step that answers a document AND a write item may write (the write item licenses it).
	e2 := d14Exec(h, ap)
	if got := e2.Execute(guardStep("run-d2", 0, StepGuard{Checklist: true, Documents: []string{"R1"}, WriteItems: []string{"R2"}}),
		draft("a@b.example", "the letter")); got.Denied {
		t.Fatalf("a step answering a write item was refused: %+v", got)
	}
	// Reads are untouched on a drafting step.
	if got := e2.Execute(guardStep("run-d2", 1, StepGuard{Checklist: true, Documents: []string{"R1"}}),
		ToolCallRequest{AgentID: "w", ToolName: "crm_search", ArgsJSON: []byte(`{}`)}); got.Denied {
		t.Fatalf("a read on a drafting step: %+v", got)
	}
}

// T-20261109-d14-constraint-forbids-change-refuses-the-send (o3x replay): "Send them
// now, exactly as drafted" → R1 write (does send) + R2 forbids change (write). The
// step carrying out R1 sends; a step that does not carry it out is still refused.
func TestD14_ConstraintNeverBlocksTheRequestsOwnWrite(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"MSG-000173"}`)}
	e := d14Exec(h, &scriptedApprover{})
	terms := map[string][]ToolEffect{"change": {EffectWrite}, "send": {EffectEgress}}
	cl := &RequirementChecklist{Items: []RequirementItem{
		{ID: "R1", Type: RequirementWrite, Text: "Send the drafts", Quote: "Send them now", Does: []string{"send"}},
		{ID: "R2", Type: RequirementConstraint, Quote: "exactly as drafted", Forbids: []string{"change"}},
	}}
	cons := ResolveConstraints(cl, terms)
	carries := CarriedClasses([]RequirementItem{cl.Items[0]}, terms)
	send := ToolCallRequest{AgentID: "w", ToolName: "mail_send", ArgsJSON: []byte(`{"to":"brisa@wychwood.example"}`)}
	if got := e.Execute(guardStep("run-s", 0, StepGuard{Checklist: true, WriteItems: []string{"R1"}, Constraints: cons, Carries: carries}), send); got.Denied {
		t.Fatalf("the send the person asked for was refused: %+v", got)
	}
	got := e.Execute(guardStep("run-s", 1, StepGuard{Checklist: true, Constraints: cons}), draft("x@y.example", "edited"))
	if !got.Denied || !strings.Contains(got.DenyReason, "exactly as drafted") {
		t.Fatalf("a write not carrying out R1 must still be held to the constraint: %+v", got)
	}
}
