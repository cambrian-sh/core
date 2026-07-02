package domain

import (
	"context"
	"testing"
	"time"
)

// The in-memory approval controller is fail-closed: no subscriber and timeout
// both deny; an approver can approve or deny a pending request (ADR-0039 D10).
func TestInMemoryApprovalController(t *testing.T) {
	ctx := context.Background()

	t.Run("no subscriber denies (fail-closed)", func(t *testing.T) {
		c := NewInMemoryApprovalController(50 * time.Millisecond)
		dec, _ := c.Request(ctx, ApprovalRequest{ToolName: "execute_command"})
		if dec.Approved {
			t.Error("with no operator subscribed, must deny")
		}
	})

	t.Run("approver approves", func(t *testing.T) {
		c := NewInMemoryApprovalController(2 * time.Second)
		reqs, cancel := c.Watch()
		defer cancel()
		go func() {
			r := <-reqs
			c.Submit(r.ID, true, "op-1")
		}()
		dec, err := c.Request(ctx, ApprovalRequest{ToolName: "execute_command"})
		if err != nil || !dec.Approved || dec.ApproverID != "op-1" {
			t.Errorf("expected approved by op-1, got %+v err=%v", dec, err)
		}
	})

	t.Run("approver denies", func(t *testing.T) {
		c := NewInMemoryApprovalController(2 * time.Second)
		reqs, cancel := c.Watch()
		defer cancel()
		go func() {
			r := <-reqs
			c.Submit(r.ID, false, "op-1")
		}()
		dec, _ := c.Request(ctx, ApprovalRequest{ToolName: "execute_command"})
		if dec.Approved {
			t.Error("operator denied, must not be approved")
		}
	})

	t.Run("timeout denies (fail-closed)", func(t *testing.T) {
		c := NewInMemoryApprovalController(40 * time.Millisecond)
		_, cancel := c.Watch() // subscribed but never submits
		defer cancel()
		dec, _ := c.Request(ctx, ApprovalRequest{ToolName: "execute_command"})
		if dec.Approved {
			t.Error("no decision within grace, must deny")
		}
	})

	t.Run("submit for unknown id is a no-op", func(t *testing.T) {
		c := NewInMemoryApprovalController(time.Second)
		if c.Submit("does-not-exist", true, "op") {
			t.Error("submitting an unknown id should return false")
		}
	})
}

var _ ApprovalController = (*InMemoryApprovalController)(nil)
