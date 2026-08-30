package domain

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The CL-2 gate battery (ADR-0127 D6/D7; HANDOFF-contribution-lane §5):
// consent enforcement pre-dispatch, the selection ladder with its
// ask-through-the-surface rung, and parking against an offline machine — all
// through the REAL executor and the REAL hub, with a chat-console stand-in
// subscribed on the consent seam. Fail-closed is the property under test: an
// effectful step without an affirmative consent path never reaches a worker.

// decisionLog is a race-safe FleetDecisions recorder (Execute runs in
// goroutines in the parking tests).
type decisionLog struct {
	mu   sync.Mutex
	list []AccessDecision
}

func (l *decisionLog) add(d AccessDecision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, d)
}

func (l *decisionLog) find(r DecisionReason) (AccessDecision, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.list {
		if d.Reason == r {
			return d, true
		}
	}
	return AccessDecision{}, false
}

func (l *decisionLog) reasons() []DecisionReason {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]DecisionReason, len(l.list))
	for i, d := range l.list {
		out[i] = d.Reason
	}
	return out
}

// consentExec wires the CL-2 executor: hub as fleet AND relay, a consent
// controller, and a decision recorder.
func consentExec(h *WorkerHub, ctrl ConsentController) (*ToolExecutor, *decisionLog) {
	e := contributionExec(h)
	e.LocalHandler = WorkerRelayHandler{Hub: h}
	e.Consent = ctrl
	log := &decisionLog{}
	e.FleetDecisions = log.add
	return e, log
}

var writeFileManifest = []SystemTool{{Name: "write_file", Description: "Write one file.", Effects: []ToolEffect{EffectRead, EffectWrite}}}

// serveSteps runs a fake worker: it polls for machine until a step arrives (or
// attempts run out), then answers with report. Returns the served step.
func serveSteps(t *testing.T, h *WorkerHub, reg WorkerRegistration,
	report func(WorkerStep)) <-chan WorkerStep {
	t.Helper()
	served := make(chan WorkerStep, 1)
	go func() {
		for i := 0; i < 40; i++ {
			step, got, err := h.PollOffer(context.Background(), reg, 0)
			if err != nil {
				return
			}
			if got {
				report(step)
				served <- step
				return
			}
		}
	}()
	return served
}

// answerPrompts subscribes the chat-console stand-in: every prompt of kind is
// answered by fn; notices and other kinds are collected but unanswered. stop
// unsubscribes and ends the responder.
func answerPrompts(ctrl *InMemoryConsentController, kind ConsentPromptKind,
	fn func(ConsentPrompt) ConsentAnswer) (seen *promptLog, stop func()) {
	ch, cancel := ctrl.Watch()
	log := &promptLog{}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case p := <-ch:
				log.add(p)
				if p.Kind == kind && fn != nil {
					ctrl.Submit(p.ID, fn(p))
				}
			case <-done:
				return
			}
		}
	}()
	return log, func() { cancel(); close(done) }
}

type promptLog struct {
	mu   sync.Mutex
	list []ConsentPrompt
}

func (l *promptLog) add(p ConsentPrompt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, p)
}

func (l *promptLog) ofKind(k ConsentPromptKind) []ConsentPrompt {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ConsentPrompt
	for _, p := range l.list {
		if p.Kind == k {
			out = append(out, p)
		}
	}
	return out
}

// waitFor spins until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── The consent hub itself: both prompt kinds through a stand-in subscriber ──

// Gate: "Both prompt kinds are received and answered through a chat-console
// stand-in" — the seam mirrors InMemoryApprovalController: raise → stream to
// subscribers → block → Submit resolves; no-subscriber and timeout fail CLOSED.
func TestConsentHub_BothPromptKindsAnsweredThroughSubscriber(t *testing.T) {
	ctrl := NewInMemoryConsentController(2 * time.Second)

	// No subscriber: fail-closed before anything is even asked.
	if ans, outcome, err := ctrl.Request(context.Background(), ConsentPrompt{Kind: ConsentPromptApprove}); err != nil ||
		outcome != ConsentNoSubscriber || ans.Approved {
		t.Fatalf("no-subscriber must fail closed: ans=%+v outcome=%s err=%v", ans, outcome, err)
	}

	ch, cancel := ctrl.Watch()
	defer cancel()
	go func() {
		for p := range ch {
			switch p.Kind {
			case ConsentPromptApprove:
				if p.Machine != "a-machine" || p.Object != "C:/secret/report.txt" {
					ctrl.Submit(p.ID, ConsentAnswer{Approved: false, AnsweredBy: "stand-in (prompt malformed)"})
					return
				}
				ctrl.Submit(p.ID, ConsentAnswer{Approved: true, AnsweredBy: "stand-in"})
			case ConsentPromptChooseMachine:
				ctrl.Submit(p.ID, ConsentAnswer{Approved: true, Machine: p.Candidates[1], AnsweredBy: "stand-in"})
			}
		}
	}()

	ans, outcome, err := ctrl.Request(context.Background(), ConsentPrompt{
		Kind: ConsentPromptApprove, Machine: "a-machine",
		Tool: "local:a-machine/write_file", Object: "C:/secret/report.txt",
	})
	if err != nil || outcome != ConsentAnswered || !ans.Approved || ans.AnsweredBy != "stand-in" {
		t.Fatalf("approve prompt: ans=%+v outcome=%s err=%v", ans, outcome, err)
	}

	ans, outcome, err = ctrl.Request(context.Background(), ConsentPrompt{
		Kind: ConsentPromptChooseMachine, Candidates: []string{"a-machine", "b-machine"}, Tool: "read_file",
	})
	if err != nil || outcome != ConsentAnswered || !ans.Approved || ans.Machine != "b-machine" {
		t.Fatalf("choose-machine prompt: ans=%+v outcome=%s err=%v", ans, outcome, err)
	}

	// Timeout with a silent subscriber: fail-closed too.
	quick := NewInMemoryConsentController(30 * time.Millisecond)
	_, cancelQuick := quick.Watch()
	defer cancelQuick()
	if ans, outcome, err := quick.Request(context.Background(), ConsentPrompt{Kind: ConsentPromptApprove}); err != nil ||
		outcome != ConsentTimedOut || ans.Approved {
		t.Fatalf("silent subscriber must time out closed: ans=%+v outcome=%s err=%v", ans, outcome, err)
	}
}

// ── Consent enforcement in the dispatch path ──

// Gate: "Read-only step under auto consent still dispatches silently but
// leaves a consent=auto decision." The subscriber is present and must see NO
// prompt (silent means silent), and the receipt lands.
func TestConsent_ReadOnlyAutoDispatchesSilentlyWithReceipt(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptApprove, nil)
	defer stop()

	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	served := serveSteps(t, h, reg, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"ok":true}`), "") })
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("read-only auto step failed: %+v", resp)
	}
	step := <-served
	if step.Consent != "" {
		t.Errorf("auto step carried a consent marker %q", step.Consent)
	}
	if dec, ok := decisions.find(ReasonConsentAuto); !ok || !dec.Allowed {
		t.Fatalf("no consent=auto receipt; decisions = %v", decisions.reasons())
	}
	if got := prompts.ofKind(ConsentPromptApprove); len(got) != 0 {
		t.Fatalf("a read-only auto step raised %d approve prompts; reads run silently", len(got))
	}
}

// The sealed ruling: anything effectful under `auto` is treated as
// any-surface — a one-tap approval routed to the surface, naming the exact
// object. Approval dispatches; the receipt names the approver.
func TestConsent_EffectfulUnderAutoRequiresApproval(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(2 * time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptApprove, func(ConsentPrompt) ConsentAnswer {
		return ConsentAnswer{Approved: true, AnsweredBy: "console"}
	})
	defer stop()

	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: writeFileManifest, Consent: ConsentAuto}
	served := serveSteps(t, h, reg, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"written":true}`), "") })
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/write_file", ArgsJSON: []byte(`{"path":"b.txt","content":"x"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("approved effectful step failed: %+v", resp)
	}
	<-served
	got := prompts.ofKind(ConsentPromptApprove)
	if len(got) != 1 {
		t.Fatalf("expected exactly one approve prompt, got %d", len(got))
	}
	p := got[0]
	if p.Machine != "a-machine" || p.Tool != "local:a-machine/write_file" {
		t.Errorf("prompt names %q on %q, want the exact step", p.Tool, p.Machine)
	}
	if p.Object != "b.txt" {
		t.Errorf("prompt object = %q, want the raw path arg verbatim", p.Object)
	}
	if !strings.Contains(p.ArgsJSON, `"content":"x"`) {
		t.Errorf("prompt does not surface the raw args: %q", p.ArgsJSON)
	}
	if dec, ok := decisions.find(ReasonConsentApproved); !ok || !dec.Allowed || !strings.Contains(dec.Detail, "console") {
		t.Fatalf("no consent_approved receipt naming the approver; decisions = %v", decisions.reasons())
	}
}

// Under an explicit any-surface knob even a READ prompts — the machine owner
// chose stricter than the sealed default.
func TestConsent_AnySurfaceKnobPromptsForReads(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(2 * time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptApprove, func(ConsentPrompt) ConsentAnswer {
		return ConsentAnswer{Approved: true, AnsweredBy: "console"}
	})
	defer stop()

	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAnySurface}
	served := serveSteps(t, h, reg, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"ok":true}`), "") })
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("approved any-surface read failed: %+v", resp)
	}
	<-served
	if len(prompts.ofKind(ConsentPromptApprove)) != 1 {
		t.Fatal("any-surface read did not prompt")
	}
	if _, ok := decisions.find(ReasonConsentApproved); !ok {
		t.Fatalf("no consent_approved receipt; decisions = %v", decisions.reasons())
	}
}

// Gate: "An effectful step without consent (denied / timeout / no subscriber)
// is refused WITH a recorded decision, and the refusal reaches the caller as
// the step's error." Each non-consent lands under its OWN reason, and the
// worker never sees the step.
func TestConsent_EffectfulWithoutConsentRefusedWithDecision(t *testing.T) {
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: writeFileManifest, Consent: ConsentAuto}

	cases := []struct {
		name       string
		grace      time.Duration
		subscribe  bool
		answer     func(ConsentPrompt) ConsentAnswer
		nilCtrl    bool
		wantReason DecisionReason
		wantDeny   string
	}{
		{
			name: "denied by the surface", grace: 2 * time.Second, subscribe: true,
			answer:     func(ConsentPrompt) ConsentAnswer { return ConsentAnswer{Approved: false, AnsweredBy: "console"} },
			wantReason: ReasonConsentDenied, wantDeny: "consent denied",
		},
		{
			name: "timeout (silent subscriber)", grace: 40 * time.Millisecond, subscribe: true,
			wantReason: ReasonConsentTimeout, wantDeny: "timed out",
		},
		{
			name: "no subscriber", grace: 40 * time.Millisecond,
			wantReason: ReasonConsentUnroutable, wantDeny: "no surface is listening",
		},
		{
			name: "no consent channel wired", nilCtrl: true,
			wantReason: ReasonConsentUnroutable, wantDeny: "no consent channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hubForTest()
			var ctrl ConsentController
			var hub *InMemoryConsentController
			if !tc.nilCtrl {
				hub = NewInMemoryConsentController(tc.grace)
				ctrl = hub
			}
			e, decisions := consentExec(h, ctrl)
			if tc.subscribe {
				_, stop := answerPrompts(hub, ConsentPromptApprove, tc.answer)
				defer stop()
			}
			// Register + keep the worker live via the liveness window; nothing
			// serves steps — none may arrive.
			if _, got, err := h.PollOffer(context.Background(), reg, time.Millisecond); err != nil || got {
				t.Fatalf("registration poll: got=%v err=%v", got, err)
			}

			ctx := WithTaskBeneficiary(context.Background(), owner)
			resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/write_file", ArgsJSON: []byte(`{"path":"b.txt"}`)})
			if !resp.Denied || !strings.Contains(resp.DenyReason, tc.wantDeny) {
				t.Fatalf("want a %q refusal reaching the caller, got %+v", tc.wantDeny, resp)
			}
			if dec, ok := decisions.find(tc.wantReason); !ok || dec.Allowed {
				t.Fatalf("no %s decision; decisions = %v", tc.wantReason, decisions.reasons())
			}
			// The step never reached the worker: the next poll answers empty.
			if step, got, _ := h.PollOffer(context.Background(), reg, 30*time.Millisecond); got {
				t.Fatalf("an unconsented step %q reached the worker", step.ID)
			}
		})
	}
}

// on-machine-only: the step dispatches carrying the wire marker (the broker
// prompts locally); a consent-denied report is a recorded REFUSAL, never a
// worker error — and a locally-approved step completes normally.
func TestConsent_OnMachineOnlyMarkerAndDeniedReport(t *testing.T) {
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: writeFileManifest, Consent: ConsentOnMachineOnly}

	t.Run("denied at the machine", func(t *testing.T) {
		h := hubForTest()
		e, decisions := consentExec(h, nil) // no kernel prompt channel needed
		served := serveSteps(t, h, reg, func(s WorkerStep) {
			_ = h.ReportOutcome("a-machine", s.ID, nil, "", true) // consent denied locally
		})
		waitLive(t, h, owner, "a-machine")

		ctx := WithTaskBeneficiary(context.Background(), owner)
		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/write_file", ArgsJSON: []byte(`{"path":"b.txt"}`)})
		step := <-served
		if step.Consent != WireConsentOnMachine {
			t.Fatalf("step consent marker = %q, want %q", step.Consent, WireConsentOnMachine)
		}
		if !resp.Denied || !strings.Contains(resp.DenyReason, "consent denied on machine") {
			t.Fatalf("a consent-denied report must be a refusal, got %+v", resp)
		}
		if _, ok := decisions.find(ReasonConsentOnMachine); !ok {
			t.Errorf("no consent_on_machine dispatch receipt; decisions = %v", decisions.reasons())
		}
		if dec, ok := decisions.find(ReasonConsentDeniedOnMachine); !ok || dec.Allowed {
			t.Fatalf("no consent_denied_on_machine refusal decision; decisions = %v", decisions.reasons())
		}
	})

	t.Run("approved at the machine", func(t *testing.T) {
		h := hubForTest()
		e, decisions := consentExec(h, nil)
		served := serveSteps(t, h, reg, func(s WorkerStep) {
			_ = h.Report("a-machine", s.ID, []byte(`{"written":true}`), "")
		})
		waitLive(t, h, owner, "a-machine")

		ctx := WithTaskBeneficiary(context.Background(), owner)
		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/write_file", ArgsJSON: []byte(`{"path":"b.txt"}`)})
		<-served
		if resp.Denied || resp.Error != "" {
			t.Fatalf("locally-approved step failed: %+v", resp)
		}
		if _, ok := decisions.find(ReasonConsentOnMachine); !ok {
			t.Fatalf("no consent_on_machine receipt; decisions = %v", decisions.reasons())
		}
	})
}

// ── The selection ladder ──

// Rung 2: a bare local:<capability> resolves to the SOLE capable live machine
// and completes end to end, receipted as machine_selected.
func TestLadder_SoleCapableLiveMachineResolves(t *testing.T) {
	h := hubForTest()
	e, decisions := consentExec(h, nil)
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	served := serveSteps(t, h, reg, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"ok":true}`), "") })
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("bare capability did not resolve: %+v", resp)
	}
	step := <-served
	if step.Machine != "a-machine" || step.Tool != "read_file" {
		t.Fatalf("resolved step = %+v", step)
	}
	if dec, ok := decisions.find(ReasonMachineSelected); !ok || !strings.Contains(dec.Detail, "sole capable") {
		t.Fatalf("no machine_selected(sole capable) receipt; decisions = %v", decisions.reasons())
	}
}

// Rung 3: with TWO capable live machines, the owner's configured default wins
// without a prompt.
func TestLadder_DefaultMachineBreaksTieWithoutPrompt(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptChooseMachine, nil)
	defer stop()

	regA := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	regB := WorkerRegistration{Machine: "b-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto, Default: true}
	servedA := serveSteps(t, h, regA, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"from":"a"}`), "") })
	servedB := serveSteps(t, h, regB, func(s WorkerStep) { _ = h.Report("b-machine", s.ID, []byte(`{"from":"b"}`), "") })
	waitLive(t, h, owner, "a-machine")
	waitLive(t, h, owner, "b-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("default-machine resolution failed: %+v", resp)
	}
	select {
	case step := <-servedB:
		if step.Machine != "b-machine" {
			t.Fatalf("step went to %q", step.Machine)
		}
	case step := <-servedA:
		t.Fatalf("step went to the non-default machine: %+v", step)
	}
	if dec, ok := decisions.find(ReasonMachineSelected); !ok || !strings.Contains(dec.Detail, "default") {
		t.Fatalf("no machine_selected(default) receipt; decisions = %v", decisions.reasons())
	}
	if len(prompts.ofKind(ConsentPromptChooseMachine)) != 0 {
		t.Fatal("a configured default still prompted")
	}
}

// Rung 4: ambiguous (two capable, no default) ⇒ the surface is ASKED and its
// answer is honored end to end.
func TestLadder_AsksThroughSurfaceAndHonorsTheAnswer(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(2 * time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptChooseMachine, func(p ConsentPrompt) ConsentAnswer {
		return ConsentAnswer{Approved: true, Machine: "b-machine", AnsweredBy: "console"}
	})
	defer stop()

	regA := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	regB := WorkerRegistration{Machine: "b-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	servedA := serveSteps(t, h, regA, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"from":"a"}`), "") })
	servedB := serveSteps(t, h, regB, func(s WorkerStep) { _ = h.Report("b-machine", s.ID, []byte(`{"from":"b"}`), "") })
	waitLive(t, h, owner, "a-machine")
	waitLive(t, h, owner, "b-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("ladder ask did not resolve: %+v", resp)
	}
	select {
	case step := <-servedB:
		if step.Machine != "b-machine" {
			t.Fatalf("step went to %q", step.Machine)
		}
	case step := <-servedA:
		t.Fatalf("step ignored the answer and went to %+v", step)
	}
	asked := prompts.ofKind(ConsentPromptChooseMachine)
	if len(asked) != 1 || len(asked[0].Candidates) != 2 {
		t.Fatalf("expected one choose-machine prompt with both candidates, got %+v", asked)
	}
	if dec, ok := decisions.find(ReasonMachineSelected); !ok || !strings.Contains(dec.Detail, "console") {
		t.Fatalf("no machine_selected(answer) receipt; decisions = %v", decisions.reasons())
	}
}

// The ladder never guesses: an unanswered "which machine?" — and a capability
// nobody live offers — refuse with a worker_unresolved decision.
func TestLadder_EndsWithoutAnswerRefusesWithDecision(t *testing.T) {
	owner := AgentPrincipal("owner-a")

	t.Run("unanswered ask", func(t *testing.T) {
		h := hubForTest()
		ctrl := NewInMemoryConsentController(40 * time.Millisecond)
		e, decisions := consentExec(h, ctrl)
		_, stop := answerPrompts(ctrl, ConsentPromptNotice, nil) // subscribed, silent
		defer stop()
		regA := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
		regB := WorkerRegistration{Machine: "b-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
		if _, got, err := h.PollOffer(context.Background(), regA, time.Millisecond); err != nil || got {
			t.Fatalf("poll a: got=%v err=%v", got, err)
		}
		if _, got, err := h.PollOffer(context.Background(), regB, time.Millisecond); err != nil || got {
			t.Fatalf("poll b: got=%v err=%v", got, err)
		}

		ctx := WithTaskBeneficiary(context.Background(), owner)
		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{}`)})
		if !resp.Denied || !strings.Contains(resp.DenyReason, "no machine could be resolved") {
			t.Fatalf("unanswered ladder must refuse, got %+v", resp)
		}
		if dec, ok := decisions.find(ReasonWorkerUnresolved); !ok || dec.Allowed {
			t.Fatalf("no worker_unresolved decision; decisions = %v", decisions.reasons())
		}
	})

	t.Run("no live capable machine", func(t *testing.T) {
		h := hubForTest()
		e, decisions := consentExec(h, NewInMemoryConsentController(time.Second))
		ctx := WithTaskBeneficiary(context.Background(), owner)
		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{}`)})
		if !resp.Denied {
			t.Fatalf("empty fleet ladder must refuse, got %+v", resp)
		}
		if _, ok := decisions.find(ReasonWorkerUnresolved); !ok {
			t.Fatalf("no worker_unresolved decision; decisions = %v", decisions.reasons())
		}
	})
}

// ── Parking ──

// parkedReg returns a hub with "a-machine" REGISTERED but offline (its
// liveness window elapsed) — the parking precondition.
func parkOffline(t *testing.T, h *WorkerHub, reg WorkerRegistration) {
	t.Helper()
	h.LivenessWindow = 20 * time.Millisecond
	if _, got, err := h.PollOffer(context.Background(), reg, time.Millisecond); err != nil || got {
		t.Fatalf("registration poll: got=%v err=%v", got, err)
	}
	waitFor(t, "the machine to fall offline", func() bool {
		return len(h.LiveFleet(context.Background(), reg.Owner)) == 0
	})
}

// Gate: "A step parked against an offline machine dispatches when the machine
// polls back in, completing end to end." The surface is notified with the
// queued-until deadline; parked + dispatched-from-park + consent=auto all land
// as decisions.
func TestParking_OfflineTargetParksThenDispatchesOnReturn(t *testing.T) {
	h := hubForTest()
	ctrl := NewInMemoryConsentController(time.Second)
	e, decisions := consentExec(h, ctrl)
	owner := AgentPrincipal("owner-a")
	prompts, stop := answerPrompts(ctrl, ConsentPromptApprove, nil)
	defer stop()

	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	parkOffline(t, h, reg)

	type result struct{ resp ToolCallResponse }
	out := make(chan result, 1)
	go func() {
		ctx := WithTaskBeneficiary(context.Background(), owner)
		out <- result{e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})}
	}()

	// The initiating surface hears about the parking, with the deadline.
	waitFor(t, "the parking notice", func() bool { return len(prompts.ofKind(ConsentPromptNotice)) > 0 })
	notice := prompts.ofKind(ConsentPromptNotice)[0]
	if !strings.Contains(notice.Notice, "offline") || !strings.Contains(notice.Notice, "queued until") {
		t.Fatalf("notice text = %q", notice.Notice)
	}
	if _, ok := decisions.find(ReasonStepParked); !ok {
		t.Fatalf("no step_parked decision; decisions = %v", decisions.reasons())
	}

	// The machine polls back in: the parked step dispatches and completes.
	served := serveSteps(t, h, reg, func(s WorkerStep) { _ = h.Report("a-machine", s.ID, []byte(`{"ok":true}`), "") })
	step := <-served
	if step.Tool != "read_file" {
		t.Fatalf("parked step served wrong tool: %+v", step)
	}
	got := <-out
	if got.resp.Denied || got.resp.Error != "" {
		t.Fatalf("parked step did not complete after the machine returned: %+v", got.resp)
	}
	for _, want := range []DecisionReason{ReasonStepParked, ReasonParkDispatched, ReasonConsentAuto} {
		if _, ok := decisions.find(want); !ok {
			t.Fatalf("missing %s decision; decisions = %v", want, decisions.reasons())
		}
	}
}

// Gate: "An expired parked step fails visibly (named error + recorded
// decision), never silently."
func TestParking_ExpiredParkFailsVisibly(t *testing.T) {
	h := hubForTest()
	e, decisions := consentExec(h, nil)
	e.ParkDeadline = 40 * time.Millisecond
	owner := AgentPrincipal("owner-a")

	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	parkOffline(t, h, reg)

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if resp.Error == "" || !strings.Contains(resp.Error, "parked step expired") ||
		!strings.Contains(resp.Error, "a-machine") {
		t.Fatalf("expiry must be a NAMED step error, got %+v", resp)
	}
	if dec, ok := decisions.find(ReasonParkExpired); !ok || dec.Allowed {
		t.Fatalf("no park_expired decision; decisions = %v", decisions.reasons())
	}
	if _, ok := decisions.find(ReasonStepParked); !ok {
		t.Fatalf("no step_parked decision; decisions = %v", decisions.reasons())
	}
}

// A fleet that cannot wait (InMemoryFleet — no LivenessWaiter) keeps the
// CL-0/CL-1 refusal for an offline target: parking never degrades to a hang on
// a source that cannot signal liveness.
func TestParking_UnavailableWithoutWaiterKeepsTheRefusal(t *testing.T) {
	e := contributionExec(ownerFleet(t, false)) // registered, NOT live, InMemoryFleet
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if !resp.Denied || !strings.Contains(resp.DenyReason, "not live") {
		t.Fatalf("offline machine on a non-waiting fleet must refuse, got %+v", resp)
	}
}

// ── The wire shape stays additive (premium-compat) ──

// The published transport tools carry the CL-2 additions additively: the step
// object gains "consent" only for on-machine-only targets, poll_step accepts
// default_machine, report_step accepts consent:"denied" — and a CL-1-shaped
// report (no consent field) still completes a step.
func TestTransport_ConsentWireFieldsAdditive(t *testing.T) {
	h := hubForTest()
	surface := ContributionLaneTools(h)
	poll, report := surface[0].Handler, surface[1].Handler
	owner := AgentPrincipal("owner-a")
	worker := WithWorkerOwner(WithPrincipal(context.Background(), MachinePrincipal("a-machine")), owner)

	// Register on-machine-only + default via the published args.
	if _, err := poll.Invoke(worker, []byte(`{"tools":[{"name":"write_file"}],"consent":"on-machine-only","default_machine":true,"wait_ms":1}`)); err != nil {
		t.Fatalf("poll_step: %v", err)
	}
	reg, known := h.RegistrationOf(context.Background(), "a-machine")
	if !known || reg.Consent != ConsentOnMachineOnly || !reg.Default {
		t.Fatalf("registration did not carry the CL-2 fields: %+v", reg)
	}

	// Dispatch: the step map carries the on-machine marker.
	errCh := make(chan error, 1)
	go func() {
		_, _, err := h.Dispatch(context.Background(), "a-machine", "write_file", []byte(`{"path":"b.txt"}`))
		errCh <- err
	}()
	var step map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for step == nil && time.Now().Before(deadline) {
		res, err := poll.Invoke(worker, []byte(`{"tools":[{"name":"write_file"}],"consent":"on-machine-only"}`))
		if err != nil {
			t.Fatalf("poll_step: %v", err)
		}
		if s, ok := res.Structured.(map[string]any)["step"].(map[string]any); ok {
			step = s
		}
	}
	if step == nil {
		t.Fatal("the held poll never returned the dispatched step")
	}
	if step["consent"] != WireConsentOnMachine {
		t.Fatalf("step consent field = %v, want %q", step["consent"], WireConsentOnMachine)
	}
	// The machine declines: report_step with consent:"denied".
	if _, err := report.Invoke(worker, []byte(`{"step_id":"`+step["id"].(string)+`","consent":"denied"}`)); err != nil {
		t.Fatalf("report_step: %v", err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "declined consent") {
		t.Fatalf("dispatch must surface the local consent denial, got %v", err)
	}

	// CL-1 compat: an auto worker whose report carries NO consent field still
	// completes a step, and its step object carries no consent key.
	if _, err := poll.Invoke(worker, []byte(`{"tools":[{"name":"write_file"}],"wait_ms":1}`)); err != nil {
		t.Fatalf("re-register auto: %v", err)
	}
	out := make(chan []byte, 1)
	go func() {
		p, _, _ := h.Dispatch(context.Background(), "a-machine", "write_file", []byte(`{}`))
		out <- p
	}()
	step = nil
	deadline = time.Now().Add(2 * time.Second)
	for step == nil && time.Now().Before(deadline) {
		res, err := poll.Invoke(worker, []byte(`{"tools":[{"name":"write_file"}]}`))
		if err != nil {
			t.Fatalf("poll_step: %v", err)
		}
		if s, ok := res.Structured.(map[string]any)["step"].(map[string]any); ok {
			step = s
		}
	}
	if step == nil {
		t.Fatal("auto worker never received its step")
	}
	if _, present := step["consent"]; present {
		t.Fatalf("auto step leaked a consent key: %+v", step)
	}
	if _, err := report.Invoke(worker, []byte(`{"step_id":"`+step["id"].(string)+`","result_json":"{\"ok\":1}"}`)); err != nil {
		t.Fatalf("CL-1-shaped report refused: %v", err)
	}
	if payload := <-out; string(payload) != `{"ok":1}` {
		t.Fatalf("CL-1-shaped report payload = %q", payload)
	}
}

// An unrecognised consent knob on the poll fails closed to any-surface — never
// to silence.
func TestPollOffer_UnknownConsentKnobFailsClosedToAnySurface(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")
	if _, got, err := h.PollOffer(context.Background(),
		WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: WorkerConsent("yolo")},
		time.Millisecond); err != nil || got {
		t.Fatalf("poll: got=%v err=%v", got, err)
	}
	reg, known := h.RegistrationOf(context.Background(), "a-machine")
	if !known || reg.Consent != ConsentAnySurface {
		t.Fatalf("unknown knob normalized to %q, want any-surface (fail-closed)", reg.Consent)
	}
}

// objectOfArgs surfaces the raw path/url verbatim and nothing else — no
// semantic parsing.
func TestObjectOfArgs_RawVerbatimOnly(t *testing.T) {
	if got := objectOfArgs([]byte(`{"path":"C:/x/y.txt","content":"z"}`)); got != "C:/x/y.txt" {
		t.Errorf("path: %q", got)
	}
	if got := objectOfArgs([]byte(`{"url":"https://e.test/a"}`)); got != "https://e.test/a" {
		t.Errorf("url: %q", got)
	}
	for _, raw := range []string{`{"query":"q"}`, `not json`, `{"path":7}`, ``} {
		if got := objectOfArgs([]byte(raw)); got != "" {
			t.Errorf("%s: derived %q, want nothing", raw, got)
		}
	}
}
