package domain

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// InMemoryApprovalController is the default ApprovalController (ADR-0039 D10).
// Request blocks until an operator submits a decision, the grace window
// elapses, or no operator is subscribed — the last two **fail closed** (deny).
// The WatchApprovals / SubmitApprovalDecision operator RPCs drive Watch / Submit.
// The controller itself is plane-agnostic; the RPC layer authenticates the
// operator plane so an agent can never reach Submit.
type InMemoryApprovalController struct {
	grace   time.Duration
	mu      sync.Mutex
	counter int
	pending map[string]chan ApprovalDecision
	subs    map[int]chan ApprovalRequest
	nextSub int
	// Bus is optional (ADR-0047 D3/0047-19): when set, a raised request publishes
	// a HITLRaisedEvent on the operator feed, keyed by the same id ResolveHITL /
	// Submit use. nil ⇒ no-op.
	Bus EventBus
}

// NewInMemoryApprovalController constructs a controller with the given fail-closed
// grace window.
func NewInMemoryApprovalController(grace time.Duration) *InMemoryApprovalController {
	return &InMemoryApprovalController{
		grace:   grace,
		pending: map[string]chan ApprovalDecision{},
		subs:    map[int]chan ApprovalRequest{},
	}
}

// Request blocks for an operator decision; denies on no-subscriber, timeout, or
// context cancellation.
func (c *InMemoryApprovalController) Request(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	c.mu.Lock()
	if len(c.subs) == 0 {
		c.mu.Unlock()
		return ApprovalDecision{Approved: false}, nil // fail-closed: no approver
	}
	c.counter++
	req.ID = fmt.Sprintf("appr-%d", c.counter)
	decCh := make(chan ApprovalDecision, 1)
	c.pending[req.ID] = decCh
	subs := make([]chan ApprovalRequest, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()

	// Publish the raise on the operator feed (same id as Submit/ResolveHITL).
	if c.Bus != nil {
		_ = c.Bus.Publish(HITLRaisedEvent{
			InterventionID: req.ID,
			AgentID:        req.AgentID,
			Description:    req.ToolName,
			IsDestructive:  true,
		})
	}

	// Deliver to subscribers (non-blocking).
	for _, s := range subs {
		select {
		case s <- req:
		default:
		}
	}

	defer func() {
		c.mu.Lock()
		delete(c.pending, req.ID)
		c.mu.Unlock()
	}()

	select {
	case dec := <-decCh:
		return dec, nil
	case <-time.After(c.grace):
		return ApprovalDecision{Approved: false}, nil // fail-closed: timeout
	case <-ctx.Done():
		return ApprovalDecision{Approved: false}, ctx.Err()
	}
}

// Watch subscribes an operator to pending approval requests. The returned cancel
// removes the subscription.
func (c *InMemoryApprovalController) Watch() (<-chan ApprovalRequest, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	ch := make(chan ApprovalRequest, 16)
	c.subs[id] = ch
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// Submit resolves a pending request. Returns false if the id is unknown
// (already resolved, timed out, or never existed).
func (c *InMemoryApprovalController) Submit(id string, approved bool, approverID string) bool {
	c.mu.Lock()
	decCh, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case decCh <- ApprovalDecision{Approved: approved, ApproverID: approverID}:
		return true
	default:
		return false
	}
}
