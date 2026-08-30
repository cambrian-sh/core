package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The CL-1 kernel-half tests: the held-poll transport. What CANNOT be tested
// here — and is deliberately absent — is the broker (a separate premium
// binary) and the full E2E gate against cmd/reference-fs-mcp, which needs it.

func hubForTest() *WorkerHub {
	h := NewWorkerHub()
	h.PollWait = 200 * time.Millisecond
	h.LivenessWindow = 300 * time.Millisecond
	h.CallTimeout = 500 * time.Millisecond
	return h
}

var readFileManifest = []SystemTool{{Name: "read_file", Description: "Read one file.", Effects: []ToolEffect{EffectRead}}}

// waitLive spins until the owner's fleet shows machine (a poll registered it)
// or the deadline passes.
func waitLive(t *testing.T, h *WorkerHub, owner PrincipalRef, machine string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, w := range h.LiveFleet(context.Background(), owner) {
			if w.Machine == machine {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker never became live")
}

// Holding the poll open IS the liveness signal (D3); after the poll returns,
// the worker stays live for the window and then drops out of every menu.
func TestWorkerHub_HeldPollDrivesLiveness(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
	}()
	waitLive(t, h, owner, "a-machine") // live while the poll is held

	if o, ok := h.OwnerOf(context.Background(), "a-machine"); !ok || o != owner {
		t.Errorf("OwnerOf = %v %v, want the registered owner", o, ok)
	}
	<-done // poll answered empty after PollWait
	if len(h.LiveFleet(context.Background(), owner)) != 1 {
		t.Fatal("worker dropped out of the fleet inside the liveness window; menus would flap on every poll gap")
	}
	time.Sleep(h.LivenessWindow + 100*time.Millisecond)
	if len(h.LiveFleet(context.Background(), owner)) != 0 {
		t.Fatal("worker still live after the window with no open poll")
	}
	// Registration (ownership) survives liveness lapse — OwnerOf is the
	// dispatch layer's fact, live or not.
	if _, ok := h.OwnerOf(context.Background(), "a-machine"); !ok {
		t.Error("registration vanished with liveness")
	}
}

// The full kernel-side relay, through the REAL executor: agent calls
// local:a-machine/read_file → hub queues the step → the held poll returns it
// → report_step completes it → the agent receives the result FENCED (D8).
func TestWorkerHub_RelayRoundTripFencesResult(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")
	e := contributionExec(h)
	e.LocalHandler = WorkerRelayHandler{Hub: h}

	// The worker: poll until a step arrives, execute "locally", report.
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				_ = h.Report("a-machine", step.ID, []byte(hostilePayload), "")
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{"path":"a.txt"}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("relay round trip failed: %+v", resp)
	}
	var env map[string]string
	if err := json.Unmarshal(resp.ResultJSON, &env); err != nil {
		t.Fatalf("result is not the fenced envelope: %v (%s)", err, resp.ResultJSON)
	}
	block := env["fenced_result"]
	if block == "" {
		t.Fatal("no fenced_result — an unfenced worker payload reached the agent (D8 violation)")
	}
	lines := strings.Split(block, "\n")
	if len(lines) != 5 || lines[1] != lines[3] {
		t.Fatalf("fence structure broken:\n%s", block)
	}
	var decoded string
	if err := json.Unmarshal([]byte(lines[2]), &decoded); err != nil || decoded != hostilePayload {
		t.Fatalf("worker payload not fenced verbatim: %v", err)
	}
}

// A worker-reported failure is fenced too, and still reads as a failure
// downstream (the envelope's kernel-authored error key).
func TestWorkerHub_WorkerFailureIsFenced(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")
	e := contributionExec(h)
	e.LocalHandler = WorkerRelayHandler{Hub: h}

	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				_ = h.Report("a-machine", step.ID, nil, hostilePayload)
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")

	ctx := WithTaskBeneficiary(context.Background(), owner)
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "agent-1", ToolName: "local:a-machine/read_file", ArgsJSON: []byte(`{}`)})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("a worker failure must be a RESULT (fenced), not a kernel error: %+v", resp)
	}
	var env map[string]string
	if err := json.Unmarshal(resp.ResultJSON, &env); err != nil {
		t.Fatalf("failure envelope not JSON: %v", err)
	}
	if env["error"] == "" || strings.Contains(env["error"], "IGNORE") {
		t.Fatalf("failure envelope error key wrong: %q", env["error"])
	}
	if !strings.Contains(env["fenced_result"], "PAYLOAD_FENCE_") {
		t.Fatal("worker failure text was not fenced")
	}
}

// Report is idempotent on the step id (handoff §6): a retried report answers
// ok and does not double-apply; a report after the call deadline answers ok
// and is discarded.
func TestWorkerHub_ReportIdempotentOnStepID(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")

	steps := make(chan WorkerStep, 1)
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				steps <- step
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")

	type dispatchOut struct {
		payload []byte
		err     error
	}
	out := make(chan dispatchOut, 1)
	go func() {
		p, _, err := h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`))
		out <- dispatchOut{p, err}
	}()
	step := <-steps
	if err := h.Report("a-machine", step.ID, []byte(`{"ok":1}`), ""); err != nil {
		t.Fatalf("first report: %v", err)
	}
	// The retry — a lost HTTP response makes this the NORMAL worker behaviour.
	if err := h.Report("a-machine", step.ID, []byte(`{"ok":2}`), ""); err != nil {
		t.Fatalf("retried report was refused: %v", err)
	}
	got := <-out
	if got.err != nil || string(got.payload) != `{"ok":1}` {
		t.Fatalf("dispatch saw %q err=%v; the FIRST report must win exactly once", got.payload, got.err)
	}

	// Deadline-abandoned step: the late report is still an idempotent ok.
	h.CallTimeout = 50 * time.Millisecond
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				time.Sleep(150 * time.Millisecond) // miss the deadline
				if rerr := h.Report("a-machine", step.ID, []byte(`{}`), ""); rerr != nil {
					t.Errorf("late report after deadline: %v (the worker did nothing wrong)", rerr)
				}
				steps <- step
				return
			}
		}
	}()
	if _, _, err := h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("deadline must fail the step visibly, got %v", err)
	}
	<-steps
}

// A step may be completed only by the machine it was dispatched to.
func TestWorkerHub_CrossMachineReportRefused(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")

	steps := make(chan WorkerStep, 1)
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				steps <- step
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")
	go func() { _, _, _ = h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`)) }()
	step := <-steps

	if err := h.Report("b-machine", step.ID, []byte(`{}`), ""); err == nil {
		t.Fatal("a foreign machine completed another machine's step")
	}
	if err := h.Report("a-machine", step.ID, []byte(`{}`), ""); err != nil {
		t.Fatalf("the step's own machine was refused: %v", err)
	}
}

// A step whose caller gave up must never reach a worker: executing it anyway
// could be an EFFECT on the consumer's machine for a plan that already failed.
func TestWorkerHub_StaleStepNeverReachesAWorker(t *testing.T) {
	h := hubForTest()
	h.CallTimeout = 30 * time.Millisecond
	owner := AgentPrincipal("owner-a")

	// Register + open the liveness window with one instant poll (no step yet).
	if _, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, time.Millisecond); err != nil || got {
		t.Fatalf("registration poll: got=%v err=%v", got, err)
	}
	// Dispatch queues the step (worker live via the window, no open poll);
	// the deadline abandons it before any poll collects it.
	if _, _, err := h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`)); err == nil {
		t.Fatal("dispatch with no polling worker must fail at the deadline")
	}
	// The next poll must NOT be handed the abandoned step.
	if step, got, _ := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 50*time.Millisecond); got {
		t.Fatalf("a stale step %q reached the worker after its caller gave up", step.ID)
	}
}

// The manifest is untrusted: malformed tool names are dropped at the hub, and
// the requested wait is clamped to the hub's bound.
func TestWorkerHub_ManifestValidationAndWaitClamp(t *testing.T) {
	h := hubForTest()
	h.PollWait = 50 * time.Millisecond
	owner := AgentPrincipal("owner-a")

	start := time.Now()
	_, got, err := h.Poll(context.Background(), "a-machine", owner, []SystemTool{
		{Name: "ok_tool"},
		{Name: "bad/name"},
		{Name: ""},
		{Name: "way" + strings.Repeat("y", 100) + "toolong"},
	}, ConsentAuto, time.Hour) // absurd wait: must be clamped
	if err != nil || got {
		t.Fatalf("poll: got=%v err=%v", got, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("requested wait was honoured (%s); the kernel must clamp the hold", elapsed)
	}
	fleet := h.LiveFleet(context.Background(), owner)
	if len(fleet) != 1 || len(fleet[0].Tools) != 1 || fleet[0].Tools[0].Name != "ok_tool" {
		t.Fatalf("manifest validation failed: %+v", fleet)
	}
}

// The published transport tools are machine-only, from the CONTEXT (INV-5):
// an agent principal, a zero principal, and a machine principal with no owner
// binding are all refused; a real worker context round-trips poll → dispatch
// → report through the published handlers.
func TestPublishedTransportTools_MachineOnlyAndRoundTrip(t *testing.T) {
	h := hubForTest()
	surface := ContributionLaneTools(h)
	poll, report := surface[0].Handler, surface[1].Handler
	for _, entry := range surface {
		if !entry.Tool.MachineOnly {
			t.Fatalf("%s is not marked MachineOnly", entry.Tool.Name)
		}
	}

	for name, ctx := range map[string]context.Context{
		"agent principal": WithPrincipal(context.Background(), AgentPrincipal("agent-1")),
		"no principal":    context.Background(),
		"machine, no owner": WithPrincipal(context.Background(),
			MachinePrincipal("a-machine")),
	} {
		if _, err := poll.Invoke(ctx, []byte(`{}`)); err == nil {
			t.Errorf("%s: poll_step did not refuse", name)
		}
		if _, err := report.Invoke(ctx, []byte(`{"step_id":"x"}`)); err == nil {
			t.Errorf("%s: report_step did not refuse", name)
		}
	}

	owner := AgentPrincipal("owner-a")
	worker := WithWorkerOwner(WithPrincipal(context.Background(), MachinePrincipal("a-machine")), owner)

	// Empty poll registers the manifest and answers step:null.
	res, err := poll.Invoke(worker, []byte(`{"tools":[{"name":"read_file","description":"Read one file.","read_only":true}],"wait_ms":1}`))
	if err != nil {
		t.Fatalf("poll_step: %v", err)
	}
	if res.Structured.(map[string]any)["step"] != nil {
		t.Fatalf("expected an empty poll, got %+v", res.Structured)
	}
	if len(h.LiveFleet(context.Background(), owner)) != 1 {
		t.Fatal("poll_step did not register the worker")
	}

	// Dispatch a step; the next poll collects it; report completes it.
	type dispatchOut struct {
		payload []byte
		err     error
	}
	out := make(chan dispatchOut, 1)
	go func() {
		p, _, err := h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{"path":"a.txt"}`))
		out <- dispatchOut{p, err}
	}()
	var step map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for step == nil && time.Now().Before(deadline) {
		res, err = poll.Invoke(worker, []byte(`{"tools":[{"name":"read_file","read_only":true}]}`))
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
	if step["tool"] != "read_file" || step["args_json"] != `{"path":"a.txt"}` {
		t.Fatalf("step shape wrong: %+v", step)
	}
	if _, err := report.Invoke(worker, []byte(`{"step_id":"`+step["id"].(string)+`","result_json":"{\"content\":\"hi\"}"}`)); err != nil {
		t.Fatalf("report_step: %v", err)
	}
	got := <-out
	if got.err != nil || string(got.payload) != `{"content":"hi"}` {
		t.Fatalf("dispatch saw %q err=%v", got.payload, got.err)
	}
}

// A completed step id is remembered FOR ITS MACHINE: the machine's own retry
// is an idempotent success, while a foreign machine gets the same answer an
// invented id gets — completion state cannot be probed across machines.
func TestWorkerHub_CompletedStepRetryIsMachineBound(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")

	steps := make(chan WorkerStep, 1)
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				steps <- step
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")
	go func() { _, _, _ = h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`)) }()
	step := <-steps

	if err := h.Report("a-machine", step.ID, []byte(`{}`), ""); err != nil {
		t.Fatalf("first report refused: %v", err)
	}
	if err := h.Report("a-machine", step.ID, []byte(`{}`), ""); err != nil {
		t.Fatalf("the machine's own retry must succeed idempotently: %v", err)
	}
	foreign := h.Report("b-machine", step.ID, []byte(`{}`), "")
	if foreign == nil {
		t.Fatal("a foreign machine confirmed another machine's completed step")
	}
	invented := h.Report("b-machine", "no-such-step", []byte(`{}`), "")
	if invented == nil {
		t.Fatal("an invented step id must be refused")
	}
	if foreign.Error() != strings.ReplaceAll(invented.Error(), "no-such-step", step.ID) {
		t.Fatalf("completed-elsewhere and never-existed must be indistinguishable: %q vs %q",
			foreign.Error(), invented.Error())
	}
}

// An oversized worker error is truncated, not refused: the waiting step still
// learns it failed, and report_step cannot smuggle an unbounded payload
// through the error field.
func TestWorkerHub_OversizedWorkerErrorTruncated(t *testing.T) {
	h := hubForTest()
	owner := AgentPrincipal("owner-a")

	steps := make(chan WorkerStep, 1)
	go func() {
		for i := 0; i < 20; i++ {
			step, got, err := h.Poll(context.Background(), "a-machine", owner, readFileManifest, ConsentAuto, 0)
			if err != nil {
				return
			}
			if got {
				steps <- step
				return
			}
		}
	}()
	waitLive(t, h, owner, "a-machine")
	type res struct {
		errMsg string
		err    error
	}
	out := make(chan res, 1)
	go func() {
		_, errMsg, err := h.Dispatch(context.Background(), "a-machine", "read_file", []byte(`{}`))
		out <- res{errMsg: errMsg, err: err}
	}()
	step := <-steps

	if err := h.Report("a-machine", step.ID, nil, strings.Repeat("x", workerErrCap+500)); err != nil {
		t.Fatalf("an oversized error must be truncated, not refused: %v", err)
	}
	got := <-out
	if got.err != nil {
		t.Fatalf("dispatch errored: %v", got.err)
	}
	if len(got.errMsg) > workerErrCap+len("… [truncated]") {
		t.Fatalf("error not bounded: %d bytes", len(got.errMsg))
	}
	if !strings.HasSuffix(got.errMsg, "… [truncated]") {
		t.Fatalf("missing truncation marker on %d-byte error", len(got.errMsg))
	}
}
