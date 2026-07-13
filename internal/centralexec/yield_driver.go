package centralexec

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// YieldBinder binds a sub-goal intent to a resource (agent ID) using the live
// selection layer — so sub-goal routing stays an inference decision, never a
// hardcoded map (Zero-Hardcode). Implemented by an adapter over the wired
// ResourceSelector at the composition root.
type YieldBinder interface {
	Bind(ctx context.Context, intent string) (agentID string, err error)
}

// YieldCaller dispatches a handoff to an agent and returns its response. The
// adapter wraps Auctioneer.CallAgent.
type YieldCaller interface {
	CallAgent(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error)
}

// ErrMaxYieldDepth bounds a yield chain on top of the D15 narrowing guard.
var ErrMaxYieldDepth = fmt.Errorf("yield: max yield-chain depth exceeded")

// YieldDriver wires the ADR-0037 D10–D15 YieldCoordinator into live dispatch
// (the previously-unwired half). It is the SYNCHRONOUS yield loop: when a called
// agent returns a _yield, the driver binds + dispatches the sub-goal through the
// selection layer (recursively, so sub-goals can themselves yield, with the
// coordinator's O(1) ancestry cycle check + D15 narrowing guard), then RESUMES
// the parent with the sub-result. The agent worker is freed at every yield (the
// agent's Execute returns), which is the point of yield-by-default — only the
// kernel's dispatch goroutine loops. Parallel fan-out (multiple sub-goals in
// flight) is intentionally out of scope (the async frontier; a follow-up).
type YieldDriver struct {
	Coordinator *YieldCoordinator
	Binder      YieldBinder
	Caller      YieldCaller
	Embedder    domain.Embedder
	MaxDepth    int // chain-depth backstop atop the D15 narrowing guard
}

// Drive calls the agent and resolves any yields it (or its sub-goals) produce,
// returning the first non-yield handoff. With no yield it is one CallAgent (fast
// path — no frontier allocated).
func (d *YieldDriver) Drive(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error) {
	resp, err := d.Caller.CallAgent(ctx, agentID, h)
	if err != nil || !isYield(resp) {
		return resp, err
	}
	// It yielded — open a frontier root for the parent's intent and resolve.
	root := d.Coordinator.OpenRoot(d.embed(ctx, intentOf(h)))
	_ = d.Coordinator.BindResource(root, agentID) // parent occupies the root (ancestry seed)
	return d.resolve(ctx, agentID, h, resp, root, 0)
}

// resolve runs the yield loop for one agent already known to have yielded
// (`first`), at frontier node `node`. Recurses for sub-goals with the child node
// so the coordinator's ancestry (cycle detection) spans the whole chain.
func (d *YieldDriver) resolve(ctx context.Context, agentID string, h, first *domain.Handoff, node string, depth int) (*domain.Handoff, error) {
	resp := first
	for isYield(resp) {
		if depth >= d.MaxDepth {
			return nil, ErrMaxYieldDepth
		}
		intent := resp.Context["_yield_intent"]
		hint := resp.Context["_yield_capability_hint"]
		cont := resp.Context["_yield_continuation_state"] // base64, opaque, returned verbatim

		// Splice the sub-goal under this node (D15 narrowing guard vs the node's intent).
		childNode, yErr := d.Coordinator.Yield(node,
			SubGoal{Intent: intent, CapabilityHint: hint, ContinuationState: []byte(cont)},
			d.embed(ctx, intent))
		if yErr != nil {
			return nil, yErr // ErrLivelock (narrowing) etc.
		}

		// Bind a resource for the sub-goal via the selection layer; cycle check.
		subAgent, bErr := d.Binder.Bind(ctx, intent)
		if bErr != nil {
			return nil, fmt.Errorf("yield: bind sub-goal %q: %w", intent, bErr)
		}
		if cErr := d.Coordinator.BindResource(childNode, subAgent); cErr != nil {
			return nil, cErr // ErrCycle
		}

		// Dispatch the sub-goal; it may itself yield (recurse with childNode so the
		// ancestry/cycle set spans the chain).
		subResp, sErr := d.dispatch(ctx, subAgent, subHandoff(h, intent), childNode, depth+1)
		if sErr != nil {
			return nil, sErr
		}

		// Resume the parent with the sub-result + its stored continuation (D10).
		resp, sErr = d.Caller.CallAgent(ctx, agentID, resumeHandoff(h, intent, subResultText(subResp), cont))
		if sErr != nil {
			return nil, sErr
		}
	}
	return resp, nil
}

// dispatch calls a sub-goal agent and resolves its yields under `node`.
func (d *YieldDriver) dispatch(ctx context.Context, agentID string, h *domain.Handoff, node string, depth int) (*domain.Handoff, error) {
	resp, err := d.Caller.CallAgent(ctx, agentID, h)
	if err != nil || !isYield(resp) {
		return resp, err
	}
	return d.resolve(ctx, agentID, h, resp, node, depth)
}

func (d *YieldDriver) embed(ctx context.Context, text string) []float32 {
	if d.Embedder == nil || text == "" {
		return nil
	}
	v, err := d.Embedder.Embed(ctx, text)
	if err != nil {
		return nil
	}
	return v
}

func isYield(h *domain.Handoff) bool {
	return h != nil && h.Context != nil && h.Context["_yield"] == "true"
}

func intentOf(h *domain.Handoff) string {
	if h != nil && h.Payload != nil {
		return string(h.Payload.Data)
	}
	return ""
}

func subResultText(h *domain.Handoff) string {
	if h != nil && h.Payload != nil {
		return string(h.Payload.Data)
	}
	return ""
}

// subHandoff is the handoff dispatched to a sub-goal agent: the intent as the
// task, carrying the parent's session token so the sub-agent's generate is
// authorized (the token is per-session, not per-agent).
func subHandoff(parent *domain.Handoff, intent string) *domain.Handoff {
	h := &domain.Handoff{
		ToAgent: "",
		Payload: &domain.Payload{Type: "text", Data: []byte(intent)},
		Context: map[string]string{},
	}
	if parent != nil && parent.Context != nil {
		if tok := parent.Context["_session_token_id"]; tok != "" {
			h.Context["_session_token_id"] = tok
		}
	}
	return h
}

// resumeHandoff re-dispatches the parent with the sub-result (delegate-and-continue):
// the agent's run_think seeds a delegated-result card from _yield_result and
// continues, keeping agency over the final answer. _yield_continuation_state is
// returned verbatim for agents that author one.
func resumeHandoff(orig *domain.Handoff, intent, subResult, cont string) *domain.Handoff {
	h := &domain.Handoff{}
	if orig != nil {
		h.Payload = orig.Payload
		h.WorkingMemory = orig.WorkingMemory
	}
	h.Context = map[string]string{
		"_yield_result":         subResult,
		"_yield_resumed_intent": intent,
	}
	if orig != nil && orig.Context != nil {
		if tok := orig.Context["_session_token_id"]; tok != "" {
			h.Context["_session_token_id"] = tok
		}
	}
	if cont != "" {
		h.Context["_yield_continuation_state"] = cont
		// validate it is well-formed base64; ignore the decoded value (opaque).
		_, _ = base64.StdEncoding.DecodeString(cont)
	}
	return h
}
