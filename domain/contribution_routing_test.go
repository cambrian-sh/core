package domain

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The CL-3 gate battery (ADR-0127 D9; slice table row CL-3): manifests enter
// the ROUTE-03 capability vocabulary, targeting is restricted to the owner's
// own fleet, and the default machine is preferred BEFORE anyone is asked.
//
// The gate sentence — "Routing selects the sole capable live machine without a
// prompt; a crafted cross-owner step is refused at selection AND at
// attachment (both layers hold)" — is proved here with the CL-0 discipline:
// each layer is proved with the OTHER one disabled through a test seam.

// neverAsk is a ConsentController that FAILS the test the moment anything asks
// it a question. "Without a prompt" is asserted at the point of raising, not
// by inspecting an empty log afterwards, so a prompt cannot pass unnoticed.
type neverAsk struct {
	t     *testing.T
	mu    sync.Mutex
	asked []ConsentPrompt
}

func (n *neverAsk) Request(_ context.Context, p ConsentPrompt) (ConsentAnswer, ConsentOutcome, error) {
	n.mu.Lock()
	n.asked = append(n.asked, p)
	n.mu.Unlock()
	n.t.Errorf("the prompt controller was asked %q about %q — routing must resolve this without prompting", p.Kind, p.Tool)
	return ConsentAnswer{}, ConsentTimedOut, nil
}

func (n *neverAsk) Notify(context.Context, ConsentPrompt) {}

func (n *neverAsk) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.asked)
}

// serveWorker runs a fake worker that keeps polling and answering until stop.
// Unlike serveSteps it serves MORE than one step, which the normalization
// tests need (two real tools, two dispatches, one capability tag).
func serveWorker(h *WorkerHub, reg WorkerRegistration, result []byte) (served <-chan WorkerStep, stop func()) {
	out := make(chan WorkerStep, 8)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			step, got, err := h.PollOffer(context.Background(), reg, 0)
			if err != nil {
				return
			}
			if got {
				_ = h.Report(reg.Machine, step.ID, result, "")
				select {
				case out <- step:
				default:
				}
			}
		}
	}()
	return out, func() { close(done) }
}

// ── Gate 1: the sole capable live machine is selected with NO prompt ──

func TestRouting_SoleCapableLiveMachineSelectedWithoutAPrompt(t *testing.T) {
	h := hubForTest()
	ask := &neverAsk{t: t}
	e, decisions := consentExec(h, ask)
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	served, stop := serveWorker(h, reg, []byte(`{"ok":true}`))
	defer stop()
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)

	// The targeting layer answers on its own, before any execution.
	tgt, reason, ok := e.targeting().Target(ctx, WorkerSelection{Capability: "read_file", AgentID: "agent-1"})
	if !ok {
		t.Fatalf("targeting refused the sole capable live machine: %s", reason)
	}
	if tgt.Machine != "a-machine" || tgt.Tool != "read_file" || tgt.ToolName != "local:a-machine/read_file" {
		t.Fatalf("target = %+v", tgt)
	}
	if !tgt.Live || !strings.Contains(tgt.Rung, "sole capable") {
		t.Fatalf("target rung/liveness = %q/%v", tgt.Rung, tgt.Live)
	}
	if tgt.Owner != owner {
		t.Fatalf("target owner = %v, want the beneficiary", tgt.Owner)
	}

	// And the step it selected runs end to end through the same ladder.
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("selected step did not run: %+v", resp)
	}
	select {
	case step := <-served:
		if step.Machine != "a-machine" || step.Tool != "read_file" {
			t.Fatalf("served step = %+v", step)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the selected machine never served the step")
	}
	if dec, found := decisions.find(ReasonMachineSelected); !found || !strings.Contains(dec.Detail, "sole capable") {
		t.Fatalf("no machine_selected receipt; decisions = %v", decisions.reasons())
	}
	if ask.count() != 0 {
		t.Fatalf("the prompt controller was called %d times", ask.count())
	}
}

// ── Gate 2: with two capable machines, the Default claimant wins, no prompt ──

func TestRouting_DefaultMachinePreferredOverPrompting(t *testing.T) {
	h := hubForTest()
	ask := &neverAsk{t: t}
	e, decisions := consentExec(h, ask)
	owner := AgentPrincipal("owner-a")
	regA := WorkerRegistration{Machine: "a-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto}
	regB := WorkerRegistration{Machine: "b-machine", Owner: owner, Tools: readFileManifest, Consent: ConsentAuto, Default: true}
	servedA, stopA := serveWorker(h, regA, []byte(`{"from":"a"}`))
	defer stopA()
	servedB, stopB := serveWorker(h, regB, []byte(`{"from":"b"}`))
	defer stopB()
	waitLive(t, h, owner, "a-machine")
	waitLive(t, h, owner, "b-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	tgt, reason, ok := e.targeting().Target(ctx, WorkerSelection{Capability: "read_file", AgentID: "agent-1"})
	if !ok {
		t.Fatalf("targeting refused with a default available: %s", reason)
	}
	if tgt.Machine != "b-machine" || !strings.Contains(tgt.Rung, "default") {
		t.Fatalf("target = %+v, want the Default-claiming machine on the default rung", tgt)
	}

	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read_file", ArgsJSON: []byte(`{}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("default-machine step did not run: %+v", resp)
	}
	select {
	case step := <-servedB:
		if step.Machine != "b-machine" {
			t.Fatalf("step went to %q", step.Machine)
		}
	case step := <-servedA:
		t.Fatalf("step went to the non-default machine: %+v", step)
	case <-time.After(2 * time.Second):
		t.Fatal("neither machine served the step")
	}
	if dec, found := decisions.find(ReasonMachineSelected); !found || !strings.Contains(dec.Detail, "default") {
		t.Fatalf("no machine_selected(default) receipt; decisions = %v", decisions.reasons())
	}
	if ask.count() != 0 {
		t.Fatalf("a configured default still prompted (%d times)", ask.count())
	}
}

// ── Gate 3: cross-owner refused at SELECTION and at ATTACHMENT, each proved
// with the other layer disabled ──

func TestRouting_CrossOwnerRefusedAtSelectionAndAtAttachment(t *testing.T) {
	// Seam A disables the ATTACHMENT layer: bypassFleet (the CL-0 seam) makes
	// LiveFleet answer with owner A's worker for ANY beneficiary, so the
	// structural menu scoping is gone and the foreign tool is visibly present.
	// The SELECTION layer must still refuse, on its own.
	t.Run("selection holds with attachment scoping bypassed", func(t *testing.T) {
		e := contributionExec(bypassFleet{real: ownerFleet(t, true)})
		log := &decisionLog{}
		e.FleetDecisions = log.add
		ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-b"))

		// Prove the bypass is real, or the proof is vacuous.
		if !menuNames(e.AvailableTools(ctx, "agent-1"))["local:a-machine/read_file"] {
			t.Fatal("bypass fleet did not leak the foreign tool; the independence proof would be vacuous")
		}

		// The routing seam sees nothing despite the leak…
		if vocab := e.ContributedVocabulary(ctx); len(vocab) != 0 {
			t.Fatalf("the routing vocabulary leaked a foreign capability: %+v", vocab)
		}
		// …and targeting refuses to aim at the machine.
		tgt, reason, ok := e.targeting().Target(ctx, WorkerSelection{Capability: "read_file", AgentID: "agent-1"})
		if ok {
			t.Fatalf("targeting selected a foreign machine: %+v", tgt)
		}
		if !strings.Contains(reason, "no machine could be resolved") {
			t.Fatalf("selection refusal reason = %q", reason)
		}
		dec, found := log.find(ReasonWorkerNotOwned)
		if !found || dec.Allowed {
			t.Fatalf("the selection layer did not record a worker_not_owned refusal; decisions = %v", log.reasons())
		}
		if !strings.Contains(dec.Detail, "refused at selection") {
			t.Fatalf("refusal is not attributed to the selection layer: %q", dec.Detail)
		}
		// The refusal must not enumerate whose fleet the machine IS in.
		if strings.Contains(dec.Detail, "owner-a") || strings.Contains(reason, "owner-a") {
			t.Fatalf("refusal names the foreign owner: detail=%q reason=%q", dec.Detail, reason)
		}
	})

	// Seam B disables the SELECTION layer completely: the step names the
	// machine EXPLICITLY, which is exactly what an attacker crafting a call
	// does — no ladder, no targeting, nothing selected. The ATTACHMENT/dispatch
	// layer must refuse alone, and the absence of any selection decision proves
	// the targeting layer was never consulted.
	t.Run("attachment holds with selection bypassed", func(t *testing.T) {
		e := contributionExec(ownerFleet(t, true)) // truthful fleet
		log := &decisionLog{}
		e.FleetDecisions = log.add
		ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-b"))

		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
		if !resp.Denied || !strings.Contains(resp.DenyReason, "not in the task beneficiary's fleet") {
			t.Fatalf("dispatch did not refuse the crafted cross-owner step: %+v", resp)
		}
		if _, selected := log.find(ReasonMachineSelected); selected {
			t.Fatal("the targeting layer ran; the selection bypass is not real")
		}
		if _, unresolved := log.find(ReasonWorkerUnresolved); unresolved {
			t.Fatal("the targeting layer ran; the selection bypass is not real")
		}
		dec, found := log.find(ReasonWorkerNotOwned)
		if !found || dec.Allowed {
			t.Fatalf("dispatch refusal not recorded; decisions = %v", log.reasons())
		}
		if strings.Contains(dec.Detail, "refused at selection") {
			t.Fatalf("this refusal came from selection, not attachment: %q", dec.Detail)
		}
	})
}

// ── Gate 4: D9 normalization — one capability tag, real names still dispatch ──

func TestNormalizeManifest_CollapsesSpellingsAndTagsTheOwner(t *testing.T) {
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{
		Machine: "a-machine",
		Owner:   owner,
		Tools: []SystemTool{
			{Name: "Read_File"},
			{Name: "list-dir"},
			{Name: "LIST_DIR"},
		},
	}
	caps := NormalizeManifest(reg)
	if len(caps) != 2 {
		t.Fatalf("manifest normalized to %d capabilities, want 2: %+v", len(caps), caps)
	}
	if caps[0].Tag != "list-dir" || caps[1].Tag != "read-file" {
		t.Fatalf("tags = %q/%q, want list-dir/read-file", caps[0].Tag, caps[1].Tag)
	}
	// The collapse: `list-dir` and `LIST_DIR` are ONE capability entry…
	if !caps[0].Ambiguous() || len(caps[0].Tools) != 2 {
		t.Fatalf("list-dir did not collapse its two spellings: %+v", caps[0])
	}
	// …while both REAL wire names are still carried, distinctly.
	if caps[0].Tools[0] != "LIST_DIR" || caps[0].Tools[1] != "list-dir" {
		t.Fatalf("real names lost: %+v", caps[0].Tools)
	}
	for _, c := range caps {
		if c.Owner != owner || c.Machine != "a-machine" {
			t.Fatalf("capability not tagged with its owner/machine: %+v", c)
		}
	}

	// The derivation happens at the DOOR, on both fleet sources, and a
	// wire-supplied value is overwritten rather than believed.
	f := NewInMemoryFleet()
	if err := f.RegisterWorker(WorkerRegistration{
		Machine:      "a-machine",
		Owner:        owner,
		Tools:        []SystemTool{{Name: "Read_File"}},
		Capabilities: []ContributedCapability{{Tag: "root", Owner: AgentPrincipal("attacker")}},
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	f.SetLive("a-machine", true)
	got := f.LiveFleet(context.Background(), owner)
	if len(got) != 1 || len(got[0].Capabilities) != 1 || got[0].Capabilities[0].Tag != "read-file" {
		t.Fatalf("InMemoryFleet did not derive the vocabulary: %+v", got)
	}
	if got[0].Capabilities[0].Owner != owner {
		t.Fatalf("wire-supplied owner tag survived: %+v", got[0].Capabilities[0])
	}

	h := hubForTest()
	if _, ok, err := h.PollOffer(context.Background(), WorkerRegistration{
		Machine:      "a-machine",
		Owner:        owner,
		Tools:        []SystemTool{{Name: "Read_File"}},
		Capabilities: []ContributedCapability{{Tag: "root", Owner: AgentPrincipal("attacker")}},
	}, time.Millisecond); err != nil || ok {
		t.Fatalf("registration poll: ok=%v err=%v", ok, err)
	}
	hreg, known := h.RegistrationOf(context.Background(), "a-machine")
	if !known || len(hreg.Capabilities) != 1 || hreg.Capabilities[0].Tag != "read-file" {
		t.Fatalf("hub did not derive the vocabulary at the poll: %+v", hreg)
	}
	if hreg.Capabilities[0].Owner != owner {
		t.Fatalf("wire-supplied owner tag survived the poll: %+v", hreg.Capabilities[0])
	}
	// The wire manifest itself is untouched — dispatch keys on the real name.
	if len(hreg.Tools) != 1 || hreg.Tools[0].Name != "Read_File" {
		t.Fatalf("the manifest was rewritten: %+v", hreg.Tools)
	}
}

// A capability tag reaches the ONE tool it names, whatever spelling the caller
// used, and the step carries the REAL wire name down to the worker.
func TestRouting_CapabilityTagReachesTheRealToolName(t *testing.T) {
	h := hubForTest()
	ask := &neverAsk{t: t}
	e, _ := consentExec(h, ask)
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{
		Machine: "a-machine",
		Owner:   owner,
		Tools:   []SystemTool{{Name: "Read_File", Effects: []ToolEffect{EffectRead}}},
		Consent: ConsentAuto,
	}
	served, stop := serveWorker(h, reg, []byte(`{"ok":true}`))
	defer stop()
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	// The caller spells it the ADR-0067 way; the machine published it another.
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:read-file", ArgsJSON: []byte(`{}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("normalized capability did not resolve: %+v", resp)
	}
	select {
	case step := <-served:
		if step.Tool != "Read_File" {
			t.Fatalf("the worker was called with %q, not its own published name", step.Tool)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no step reached the worker")
	}
	if ask.count() != 0 {
		t.Fatalf("prompted %d times", ask.count())
	}
}

// The collapse must never make two DIFFERENT tools on ONE machine
// indistinguishable at dispatch: the ambiguous TAG resolves nothing (never a
// guess), while either exact wire name still dispatches to its own tool.
func TestRouting_AmbiguousTagRefusesWhileExactNamesDispatch(t *testing.T) {
	h := hubForTest()
	e, decisions := consentExec(h, nil)
	owner := AgentPrincipal("owner-a")
	reg := WorkerRegistration{
		Machine: "a-machine",
		Owner:   owner,
		Tools: []SystemTool{
			{Name: "read_file", Effects: []ToolEffect{EffectRead}},
			{Name: "read-file", Effects: []ToolEffect{EffectRead}},
		},
		Consent: ConsentAuto,
	}
	served, stop := serveWorker(h, reg, []byte(`{"ok":true}`))
	defer stop()
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)

	// A spelling that is nobody's exact name resolves through the tag — and
	// the tag names two different tools, so it resolves NOTHING.
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:Read File", ArgsJSON: []byte(`{}`)})
	if !resp.Denied || !strings.Contains(resp.DenyReason, "more than one tool") {
		t.Fatalf("an ambiguous tag was resolved instead of refused: %+v", resp)
	}
	if _, found := decisions.find(ReasonWorkerUnresolved); !found {
		t.Fatalf("no worker_unresolved decision; decisions = %v", decisions.reasons())
	}

	// Both real names still dispatch, each to itself.
	for _, want := range []string{"read_file", "read-file"} {
		resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/" + want, ArgsJSON: []byte(`{}`)})
		if resp.Denied || resp.Error != "" {
			t.Fatalf("exact name %q did not dispatch: %+v", want, resp)
		}
		select {
		case step := <-served:
			if step.Tool != want {
				t.Fatalf("exact name %q dispatched as %q", want, step.Tool)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no step reached the worker for %q", want)
		}
	}
}

// ── Gate 5: the routing vocabulary is owner-scoped and fail-closed ──

func TestContributedVocabulary_OwnerScopedAndFailClosed(t *testing.T) {
	f := NewInMemoryFleet()
	for _, reg := range []WorkerRegistration{
		{Machine: "a-machine", Owner: AgentPrincipal("owner-a"), Tools: readFileManifest},
		{Machine: "b-machine", Owner: AgentPrincipal("owner-b"), Tools: writeFileManifest},
	} {
		if err := f.RegisterWorker(reg); err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
		f.SetLive(reg.Machine, true)
	}
	e := contributionExec(f)

	vocab := e.ContributedVocabulary(WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a")))
	if len(vocab) != 1 || vocab[0].Tag != "read-file" || vocab[0].Machine != "a-machine" {
		t.Fatalf("owner A's vocabulary = %+v", vocab)
	}
	if vocab[0].Owner != AgentPrincipal("owner-a") {
		t.Fatalf("vocabulary entry is not owner-tagged: %+v", vocab[0])
	}

	for name, ctx := range map[string]context.Context{
		"no beneficiary": context.Background(),
		"zero principal": WithTaskBeneficiary(context.Background(), PrincipalRef{}),
		"stranger":       WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-c")),
	} {
		if got := e.ContributedVocabulary(ctx); len(got) != 0 {
			t.Errorf("%s: routing vocabulary was not empty: %+v", name, got)
		}
	}
	e.Fleet = nil
	if got := e.ContributedVocabulary(WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))); len(got) != 0 {
		t.Errorf("no fleet source: routing vocabulary was not empty: %+v", got)
	}
}

// An offline machine is registered but contributes nothing to ROUTING either —
// the D6 live-only rule holds at the targeting seam, not just in menus.
func TestContributedVocabulary_OfflineMachineIsNotRoutable(t *testing.T) {
	e := contributionExec(ownerFleet(t, false))
	ctx := WithTaskBeneficiary(context.Background(), AgentPrincipal("owner-a"))
	if got := e.ContributedVocabulary(ctx); len(got) != 0 {
		t.Fatalf("an offline machine is in the routing vocabulary: %+v", got)
	}
}
