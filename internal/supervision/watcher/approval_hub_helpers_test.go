package watcher

import (
	"context"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// opRequests subscribes to the addressed approval hub as an operator and yields
// the raised requests in the ApprovalRequest shape (ADR-0142 D11). The returned
// cancel ends the subscription and the relay goroutine (idempotent).
func opRequests(h *domain.AddressedHub) (<-chan domain.ApprovalRequest, func()) {
	evs, cancel := h.Watch(domain.ApprovalWatch{For: domain.OperatorApprover("op")})
	out := make(chan domain.ApprovalRequest, 16)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case ev := <-evs:
				if ev.Type != domain.ApprovalEventRaised {
					continue
				}
				it := ev.Item
				select {
				case out <- domain.ApprovalRequest{ID: it.ID, AgentID: it.AgentID, ToolName: it.ToolName, ArgsPreview: it.ArgsPreview, Kind: it.Kind, Timeout: it.ExpiresAt.Sub(it.RaisedAt)}:
				case <-done:
					return
				}
			}
		}
	}()
	var once sync.Once
	return out, func() { once.Do(func() { cancel(); close(done) }) }
}

// opSubmit answers an approval as operator op; false when refused or unknown.
func opSubmit(h *domain.AddressedHub, id string, approve bool, op string) bool {
	_, err := h.Answer(context.Background(), id, domain.ApprovalAnswer{Approved: approve, By: domain.OperatorApprover(op)})
	return err == nil
}
