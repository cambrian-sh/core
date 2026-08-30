package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// The CL-1 kernel half of the contribution lane (ADR-0127 D3/D5/D6): the
// worker hub. Pull is the ONLY transport — the kernel never pushes, never
// dials a consumer machine, never holds an address for one. A worker holds
// `poll_step` open; holding it open IS the liveness signal; a step for that
// worker is returned as the response body of a request the worker itself
// opened; `report_step` completes it. Between polls the worker stays live for
// a small multiple of the poll wait, so menus do not flap on the poll gap.
//
// The hub is also the lane's live FleetSource: registration is DECLARATIVE —
// the manifest rides every poll (D3 names exactly two verbs, so offering is
// idempotent re-statement, which also makes the fleet self-healing across
// kernel restarts: registrations live in memory, credentials live in the
// ADR-0101 store, and the next poll rebuilds the one from the other).

// Hub defaults. Fields on WorkerHub override them; they are constants rather
// than config keys in this slice (a new knob is a change-control event — the
// CL-2/CL-3 slices decide which of these deserve one).
const (
	// defaultPollWait is how long poll_step holds with no step before
	// answering empty. Under the endpoint's HTTP transport a longer hold is a
	// bet on every proxy's idle timeout.
	defaultPollWait = 25 * time.Second
	// defaultLivenessWindow is how long a worker stays live AFTER its poll
	// returns — the "small multiple of the poll timeout" of D3, covering the
	// re-issue gap so a healthy worker's tools do not flap out of menus.
	defaultLivenessWindow = 75 * time.Second
	// defaultCallTimeout bounds one relayed call (review §4.3: a contributed
	// call carries its own deadline; a worker that never answers must fail the
	// STEP visibly, never hang the plan).
	defaultCallTimeout = 60 * time.Second
	// defaultMaxResultBytes bounds one report_step payload; the executor's
	// InlineThreshold offloads big results downstream, but an unbounded accept
	// here would let one worker exhaust kernel memory.
	defaultMaxResultBytes = 4 << 20
	// stepQueueDepth bounds undelivered steps per worker; a worker that is
	// live but not draining is a stalled worker, and queueing deeper only
	// converts that fact into latency.
	stepQueueDepth = 64
	// completedRetention bounds the remembered step ids that make report_step
	// idempotent (the DW-era dedup lesson: a retried report must not
	// double-apply).
	completedRetention = 1024
)

// WorkerStep is one relayed invocation handed to a worker: the BARE tool name
// (the worker executes against its local MCP server; the local: namespacing is
// kernel-side), the args, and the id report_step must echo.
type WorkerStep struct {
	ID       string
	Machine  string
	Tool     string
	ArgsJSON []byte
	// Consent carries WireConsentOnMachine when the machine's D7 knob is
	// on-machine-only: the broker must obtain local consent before executing,
	// and reports a refusal via the report's consent field. Empty otherwise.
	// ADDITIVE on the wire — a worker that ignores unknown step fields is
	// untouched (CL-2).
	Consent string
}

// workerToolName bounds manifest tool names to the shape every downstream
// name-validator accepts (ADR-0097 D7 — this codebase has lost the loose-name
// bet once already).
var workerToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type stepResult struct {
	payload []byte
	errMsg  string
	// consentDenied marks a report whose consent field said the machine
	// declined the step (on-machine-only knob) — a refusal, not a worker error.
	consentDenied bool
}

type pendingStep struct {
	machine string
	ch      chan stepResult
}

type hubWorker struct {
	reg       WorkerRegistration
	polling   int
	liveUntil time.Time
	queue     chan WorkerStep
}

// WorkerHub is the held-poll transport core. Safe for concurrent use. The
// zero value is not usable; construct with NewWorkerHub.
type WorkerHub struct {
	// PollWait / LivenessWindow / CallTimeout / MaxResultBytes override the
	// package defaults when set; tests set them small.
	PollWait       time.Duration
	LivenessWindow time.Duration
	CallTimeout    time.Duration
	MaxResultBytes int

	mu             sync.Mutex
	workers        map[string]*hubWorker
	pending        map[string]*pendingStep
	completed      map[string]string // step id → machine that completed (or abandoned) it
	completedOrder []string
	// parked holds the CL-2 parking waiters per machine, in arrival order: a
	// step whose target was offline when it arrived blocks here until the
	// machine's next poll wakes it (FIFO) or its park deadline expires. This is
	// in-memory bookkeeping with the hub's own lifetime — parked steps do not
	// survive a kernel restart (the phase-4 residual, stated in the ADR).
	parked map[string][]chan struct{}
}

// NewWorkerHub constructs an empty hub.
func NewWorkerHub() *WorkerHub {
	return &WorkerHub{
		workers:   map[string]*hubWorker{},
		pending:   map[string]*pendingStep{},
		completed: map[string]string{},
		parked:    map[string][]chan struct{}{},
	}
}

func (h *WorkerHub) pollWait() time.Duration {
	if h.PollWait > 0 {
		return h.PollWait
	}
	return defaultPollWait
}

func (h *WorkerHub) livenessWindow() time.Duration {
	if h.LivenessWindow > 0 {
		return h.LivenessWindow
	}
	return defaultLivenessWindow
}

func (h *WorkerHub) callTimeout() time.Duration {
	if h.CallTimeout > 0 {
		return h.CallTimeout
	}
	return defaultCallTimeout
}

func (h *WorkerHub) maxResultBytes() int {
	if h.MaxResultBytes > 0 {
		return h.MaxResultBytes
	}
	return defaultMaxResultBytes
}

// Poll is the CL-1 long-poll shape, kept for its call sites; it delegates to
// PollOffer with the registration it implies (no default-machine claim).
func (h *WorkerHub) Poll(ctx context.Context, machine string, owner PrincipalRef,
	tools []SystemTool, consent WorkerConsent, wait time.Duration) (WorkerStep, bool, error) {
	return h.PollOffer(ctx, WorkerRegistration{Machine: machine, Owner: owner, Tools: tools, Consent: consent}, wait)
}

// PollOffer is the long-poll: it (re-)registers the worker from the offered
// registration, holds until a step arrives or wait elapses, and returns the
// step (ok=false ⇒ answered empty). Machine and Owner come from the
// AUTHENTICATED context at the published-tool handler — never from arguments
// (INV-5); the rest of the offer (manifest, consent knob, default-machine
// claim) is the worker's own declarative state, validated here.
//
// wait ≤ 0 or beyond the hub bound is clamped to the hub's PollWait: the
// worker proposes, the kernel disposes, because the hold rides an HTTP
// response the kernel is accountable for.
func (h *WorkerHub) PollOffer(ctx context.Context, offer WorkerRegistration, wait time.Duration) (WorkerStep, bool, error) {
	machine := offer.Machine
	if machine == "" || offer.Owner.IsZero() {
		return WorkerStep{}, false, errors.New("worker hub: a poll needs an authenticated machine and its owner principal")
	}
	kept := make([]SystemTool, 0, len(offer.Tools))
	for _, t := range offer.Tools {
		if !workerToolName.MatchString(t.Name) {
			// Dropped, not fatal: one malformed manifest entry must not
			// unregister a worker's whole fleet membership.
			continue
		}
		kept = append(kept, t)
	}
	offer.Tools = kept
	// D9/CL-3: the manifest enters the ROUTE-03 capability vocabulary HERE —
	// the moment it enters the kernel — normalized and owner-tagged. Derived,
	// never trusted: whatever the wire said is overwritten.
	offer.Capabilities = NormalizeManifest(offer)
	// The consent knob is a closed set. Empty defaults to auto (the sealed
	// read-only default); an UNRECOGNISED value fails closed to any-surface —
	// a kernel-routed prompt with deny as the default — never to silence.
	switch offer.Consent {
	case ConsentAuto, ConsentAnySurface, ConsentOnMachineOnly:
	case "":
		offer.Consent = ConsentAuto
	default:
		offer.Consent = ConsentAnySurface
	}
	if wait <= 0 || wait > h.pollWait() {
		wait = h.pollWait()
	}

	h.mu.Lock()
	w, ok := h.workers[machine]
	if !ok {
		w = &hubWorker{queue: make(chan WorkerStep, stepQueueDepth)}
		h.workers[machine] = w
	}
	// The manifest rides the poll, so registration is a re-statement: the
	// OWNER comes from the durable credential binding each request, which is
	// what lets a rotate --owner re-point a machine without a kernel restart.
	w.reg = offer
	w.polling++
	queue := w.queue
	// The machine is live NOW (an open poll IS liveness): wake any steps that
	// parked while it was offline, in arrival order (ADR-0127 D6). Each woken
	// step re-enters the normal consent-checked dispatch path.
	h.wakeParkedLocked(machine)
	h.mu.Unlock()

	release := func() {
		h.mu.Lock()
		w.polling--
		w.liveUntil = time.Now().Add(h.livenessWindow())
		h.mu.Unlock()
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case step := <-queue:
			// A step whose caller has already given up (call deadline, plan
			// canceled) must NOT reach the worker: executing it anyway could
			// be an EFFECT on the consumer's machine for a plan that already
			// failed. Skipped, not returned — the deadline did its job.
			h.mu.Lock()
			_, stillWanted := h.pending[step.ID]
			h.mu.Unlock()
			if !stillWanted {
				continue
			}
			release()
			return step, true, nil
		case <-timer.C:
			release()
			return WorkerStep{}, false, nil
		case <-ctx.Done():
			release()
			return WorkerStep{}, false, ctx.Err()
		}
	}
}

// live reports whether a worker can serve a step right now: a poll is open,
// or the last one returned within the liveness window. Callers hold h.mu.
func (w *hubWorker) live(now time.Time) bool {
	return w.polling > 0 || now.Before(w.liveUntil)
}

// ErrParkExpired ends a parked step whose deadline passed with the machine
// still offline — the visible, named failure D6 requires (never silence).
var ErrParkExpired = errors.New("worker hub: parked step deadline expired")

// ErrWorkerConsentDenied marks a step the machine itself declined
// (on-machine-only consent, ADR-0127 D7): a refusal to record, not a worker
// error to relay. The text is KERNEL-authored — nothing worker-written rides
// this error, so it needs no D8 fence.
var ErrWorkerConsentDenied = errors.New("worker declined consent")

// AwaitLive blocks until machine is live, wait elapses (ErrParkExpired), or
// ctx is done — the D6 parking primitive (LivenessWaiter). Waiters wake in
// arrival order when the machine's next poll registers it live. Parking is for
// steps that ARRIVE while the machine is already offline; a machine that drops
// mid-step is the call deadline's business (CL-1), not this one's.
func (h *WorkerHub) AwaitLive(ctx context.Context, machine string, wait time.Duration) error {
	if wait <= 0 {
		return ErrParkExpired // a zero window parks nothing
	}
	h.mu.Lock()
	if w, ok := h.workers[machine]; ok && w.live(time.Now()) {
		h.mu.Unlock()
		return nil
	}
	woken := make(chan struct{})
	h.parked[machine] = append(h.parked[machine], woken)
	h.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-woken:
		return nil
	case <-timer.C:
		if !h.removeParked(machine, woken) {
			return nil // woken concurrently with the timer: the machine IS live
		}
		return ErrParkExpired
	case <-ctx.Done():
		h.removeParked(machine, woken)
		return ctx.Err()
	}
}

// wakeParkedLocked wakes machine's parked waiters in arrival order. Callers
// hold h.mu.
func (h *WorkerHub) wakeParkedLocked(machine string) {
	waiters := h.parked[machine]
	if len(waiters) == 0 {
		return
	}
	delete(h.parked, machine)
	for _, ch := range waiters {
		close(ch)
	}
}

// removeParked withdraws one waiter; false means it had already been woken.
func (h *WorkerHub) removeParked(machine string, ch chan struct{}) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	waiters := h.parked[machine]
	for i, c := range waiters {
		if c == ch {
			h.parked[machine] = append(waiters[:i:i], waiters[i+1:]...)
			if len(h.parked[machine]) == 0 {
				delete(h.parked, machine)
			}
			return true
		}
	}
	return false
}

// Dispatch relays one authorized call to a worker and waits for its report —
// the WorkerRelayHandler's engine. The executor is the reference monitor and
// has already resolved ownership and liveness; this re-checks liveness
// fail-closed (the worker may have dropped between resolution and dispatch)
// and NEVER hangs: no report within the call deadline fails the step visibly
// (review §4.3 — parking with a deadline is CL-2, on the task lane).
//
// The three returns keep the trust boundary visible at the call site: payload
// and workerErr are the WORKER'S words (untrusted — the relay fences both,
// D8); err is the KERNEL'S own diagnosis (not live, deadline, backlog) and
// safe to surface as-is.
func (h *WorkerHub) Dispatch(ctx context.Context, machine, tool string, argsJSON []byte) (payload []byte, workerErr string, err error) {
	id, err := stepID()
	if err != nil {
		return nil, "", fmt.Errorf("worker hub: mint step id: %w", err)
	}
	step := WorkerStep{ID: id, Machine: machine, Tool: tool, ArgsJSON: argsJSON}

	h.mu.Lock()
	w, ok := h.workers[machine]
	if !ok || !w.live(time.Now()) {
		h.mu.Unlock()
		return nil, "", fmt.Errorf("worker hub: machine %q is not live", machine)
	}
	// D7 on-machine-only: the step carries the consent marker so the broker
	// prompts locally before executing (additive wire field; the consent
	// outcome comes back on the report).
	if w.reg.Consent == ConsentOnMachineOnly {
		step.Consent = WireConsentOnMachine
	}
	ps := &pendingStep{machine: machine, ch: make(chan stepResult, 1)}
	h.pending[id] = ps
	select {
	case w.queue <- step:
	default:
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, "", fmt.Errorf("worker hub: machine %q has %d undelivered steps; refusing to queue deeper", machine, stepQueueDepth)
	}
	h.mu.Unlock()

	timer := time.NewTimer(h.callTimeout())
	defer timer.Stop()
	select {
	case res := <-ps.ch:
		if res.consentDenied {
			// The machine declined the step (on-machine-only knob). A KERNEL
			// error, deliberately: the refusal is the kernel's own diagnosis to
			// record, and no worker-authored text rides it.
			return nil, "", fmt.Errorf("worker hub: machine %q declined consent for step %s: %w",
				machine, id, ErrWorkerConsentDenied)
		}
		return res.payload, res.errMsg, nil
	case <-timer.C:
		h.abandon(id)
		return nil, "", fmt.Errorf("worker hub: machine %q did not answer step %s within %s", machine, id, h.callTimeout())
	case <-ctx.Done():
		h.abandon(id)
		return nil, "", ctx.Err()
	}
}

// abandon closes out a step nobody is waiting for any more, remembering the
// id so a LATE report answers idempotently instead of "unknown step" — the
// worker did nothing wrong; the deadline did its job.
func (h *WorkerHub) abandon(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ps, ok := h.pending[id]; ok {
		delete(h.pending, id)
		h.remember(id, ps.machine)
	}
}

// workerErrCap bounds the error string a worker may attach to a report. An
// error is a message, not a payload: past the cap it is truncated rather than
// refused, so the waiting step still learns that — and roughly why — it
// failed. (The result bytes stay refuse-over-cap; truncated JSON is garbage.)
const workerErrCap = 4096

// Report completes one step (the CL-1 shape; no consent outcome).
func (h *WorkerHub) Report(machine, stepID string, result []byte, workerErr string) error {
	return h.ReportOutcome(machine, stepID, result, workerErr, false)
}

// ReportOutcome completes one step. Idempotent on the step id (a retried
// report_step must not double-apply — the DW-era lesson), and a step may be
// completed — or even confirmed to exist — ONLY by the machine it was
// dispatched to: workers cannot answer for each other, and a foreign machine
// gets the same "unknown step" answer whether the id is pending, completed, or
// invented, so step ids cannot be enumerated across machines.
//
// consentDenied is the CL-2 addition: the machine declined the step under its
// on-machine-only knob. It surfaces from Dispatch as ErrWorkerConsentDenied —
// a recorded refusal, never a worker error.
func (h *WorkerHub) ReportOutcome(machine, stepID string, result []byte, workerErr string, consentDenied bool) error {
	if len(result) > h.maxResultBytes() {
		return fmt.Errorf("worker hub: result exceeds %d bytes", h.maxResultBytes())
	}
	if len(workerErr) > workerErrCap {
		workerErr = workerErr[:workerErrCap] + "… [truncated]"
	}
	h.mu.Lock()
	ps, ok := h.pending[stepID]
	if !ok {
		doneMachine, done := h.completed[stepID]
		h.mu.Unlock()
		if done && doneMachine == machine {
			return nil // already applied (or deadline-abandoned); a retry is a success
		}
		return fmt.Errorf("worker hub: unknown step %q", stepID)
	}
	if ps.machine != machine {
		h.mu.Unlock()
		return fmt.Errorf("worker hub: unknown step %q", stepID)
	}
	delete(h.pending, stepID)
	h.remember(stepID, machine)
	h.mu.Unlock()

	ps.ch <- stepResult{payload: result, errMsg: workerErr, consentDenied: consentDenied} // buffered; never blocks
	return nil
}

// remember records a completed step id and the machine it belonged to,
// bounded FIFO. Callers hold h.mu.
func (h *WorkerHub) remember(id, machine string) {
	if _, ok := h.completed[id]; ok {
		return
	}
	h.completed[id] = machine
	h.completedOrder = append(h.completedOrder, id)
	if len(h.completedOrder) > completedRetention {
		evict := h.completedOrder[0]
		h.completedOrder = h.completedOrder[1:]
		delete(h.completed, evict)
	}
}

// ── FleetSource ───────────────────────────────────────────────────────────────

// LiveFleet returns the live workers owned by owner (zero owner ⇒ nil,
// fail-closed) — the same contract InMemoryFleet serves in tests, driven here
// by the held poll (D6: live-only advertisement, the REC-02 lesson).
func (h *WorkerHub) LiveFleet(_ context.Context, owner PrincipalRef) []WorkerRegistration {
	if owner.IsZero() {
		return nil
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []WorkerRegistration
	for _, w := range h.workers {
		if w.live(now) && w.reg.Owner == owner {
			out = append(out, w.reg)
		}
	}
	// Machine order is map order otherwise, so the same fleet produced a
	// differently-ordered tool menu on every build — which reshuffles the menu
	// an agent is shown between two identical turns. Sort by machine name.
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

// OwnerOf returns a machine's registered owner, live or not — the dispatch
// layer's independent ownership fact.
func (h *WorkerHub) OwnerOf(_ context.Context, machine string) (PrincipalRef, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w, ok := h.workers[machine]; ok {
		return w.reg.Owner, true
	}
	return PrincipalRef{}, false
}

// RegistrationOf returns a machine's last registration, live or not
// (WorkerRegistry) — parking resolves an offline machine's manifest through it.
func (h *WorkerHub) RegistrationOf(_ context.Context, machine string) (WorkerRegistration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w, ok := h.workers[machine]; ok {
		return w.reg, true
	}
	return WorkerRegistration{}, false
}

// RegisteredFleet returns ALL of owner's registered workers, live or not
// (WorkerRegistry) — ladder rung 3 finds an offline default machine through
// it. Zero owner ⇒ nil, fail-closed.
func (h *WorkerHub) RegisteredFleet(_ context.Context, owner PrincipalRef) []WorkerRegistration {
	if owner.IsZero() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []WorkerRegistration
	for _, w := range h.workers {
		if w.reg.Owner == owner {
			out = append(out, w.reg)
		}
	}
	return out
}

var (
	_ FleetSource    = (*WorkerHub)(nil)
	_ WorkerRegistry = (*WorkerHub)(nil)
	_ LivenessWaiter = (*WorkerHub)(nil)
)

// stepID mints one step's identity: 64 random bits, hex. Random rather than a
// counter for the same reason the D6 correlation handle is — ids reach an
// external worker, and a sequence discloses traffic volume.
func stepID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "step-" + hex.EncodeToString(b[:]), nil
}

// WorkerRelayHandler is the CL-1 dispatch handler for local:<machine>/<tool>
// steps: relay through the hub, then FENCE the result before it reaches any
// agent context (D8 — ships in the first slice that relays a result, which is
// this one, and has no exceptions). It replaces LocalRelayStub in the
// composition root once a hub exists.
type WorkerRelayHandler struct {
	Hub *WorkerHub
}

// Execute relays one already-authorized call. The executor above is the
// reference monitor — ownership, liveness, grants, effects and budget have
// all held before this runs; this only carries the step down and the fenced
// result up.
//
// BOTH worker-authored outcomes are fenced — the result and the error alike
// (D8 covers the whole report_step payload; a hostile local server does not
// get an unfenced channel just by labelling its text an error). Only the
// kernel's own errors (not live, deadline) return as Go errors.
func (r WorkerRelayHandler) Execute(ctx context.Context, call ToolCall) ([]byte, error) {
	if r.Hub == nil {
		return nil, errors.New("contribution lane: no worker hub configured")
	}
	machine, bare, ok := SplitContributedToolName(call.ToolName)
	if !ok {
		return nil, fmt.Errorf("contribution lane: %q is not a contributed tool name", call.ToolName)
	}
	raw, workerErr, err := r.Hub.Dispatch(ctx, machine, bare, call.ArgsJSON)
	if err != nil {
		return nil, err
	}
	if workerErr != "" {
		return FenceWorkerFailure(machine, workerErr), nil
	}
	return FenceWorkerResult(machine, raw), nil
}
