package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// DaemonCaller is the narrow slice of the kernel's daemon dispatcher this needs.
// It is satisfied by the same seam the chat tier uses (`CallDaemon`), taken as an
// interface so the delivery path does not depend on the whole agent manager.
type DaemonCaller interface {
	CallDaemon(ctx context.Context, streamID string, h *domain.Handoff) (*domain.Handoff, error)
}

// DaemonTransport delivers an outbound message by calling the ingress daemon.
//
// This is the only place the abstract "hand it to the ingress" becomes a concrete
// process call, and it is deliberately thin: it addresses the daemon, hands over
// the payload, and translates the daemon's answer into the permanent/transient
// vocabulary the delivery path understands. It makes no decisions about who the
// recipient is — that was settled before this is reached (ADR-0090 D7).
type DaemonTransport struct {
	caller DaemonCaller
	// streamFor maps an ingress agent id to the daemon stream that serves it. An
	// ingress is ONE long-lived daemon, unlike a per-conversation session daemon,
	// so the default is the agent id itself. Overridable rather than hardcoded,
	// because a deployment that runs several instances of one ingress will need a
	// different rule and should not have to fork this.
	streamFor func(ingressAgentID string) string
}

// NewDaemonTransport wires delivery to the kernel's daemon dispatcher.
func NewDaemonTransport(caller DaemonCaller) *DaemonTransport {
	return &DaemonTransport{
		caller:    caller,
		streamFor: func(agentID string) string { return agentID },
	}
}

// SetStreamResolver overrides how an ingress agent id maps to a daemon stream.
func (t *DaemonTransport) SetStreamResolver(f func(ingressAgentID string) string) {
	if f != nil {
		t.streamFor = f
	}
}

// deliveryPayload is what the ingress daemon receives. It is JSON in the handoff
// payload rather than new proto fields, so adding an ingress does not move the
// pinned contract — the same reasoning that keeps daemon params opaque.
type deliveryPayload struct {
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	Recipient      string `json:"recipient"`
	Text           string `json:"text"`
	TxnID          string `json:"txn_id,omitempty"`
	// Final marks the last progress update of a turn: render it, then stop tracking the
	// line so the next turn starts a fresh one instead of overwriting this.
	Final bool `json:"final,omitempty"`
}

// Deliver hands one message to the ingress daemon.
func (t *DaemonTransport) Deliver(ctx context.Context, d domain.IngressDelivery) error {
	if t.caller == nil {
		return errors.New("delivery: no daemon caller configured")
	}

	kind := string(d.Kind.Resolved())
	body, err := json.Marshal(deliveryPayload{
		Kind:           kind,
		ConversationID: d.ConversationID,
		Recipient:      d.Address.ExternalID,
		Text:           d.Text,
		TxnID:          d.TxnID,
		Final:          d.Final,
	})
	if err != nil {
		return fmt.Errorf("delivery: encode payload: %w", err)
	}

	h := &domain.Handoff{
		ID:        d.TxnID,
		FromAgent: "kernel",
		ToAgent:   d.Address.IngressAgentID,
		Payload:   &domain.Payload{ID: d.TxnID, Type: kind, Data: body},
		Context: map[string]string{
			"_conversation_id": d.ConversationID,
			"_delivery_txn":    d.TxnID,
		},
	}

	resp, err := t.caller.CallDaemon(ctx, t.streamFor(d.Address.IngressAgentID), h)
	if err != nil {
		// A transport-level failure is transient by default. The daemon may be
		// restarting (ADR-0070 supervises it), and assuming permanence here would
		// dead-letter messages that a retry seconds later would deliver.
		return fmt.Errorf("delivery: calling ingress %s: %w", d.Address.IngressAgentID, err)
	}
	return interpret(resp)
}

// interpret turns the daemon's answer into the delivery vocabulary.
//
// Only the ingress knows whether "the user blocked this bot" is permanent, so it
// says so and anything unlabelled is treated as transient — the recoverable
// assumption, since wrongly retrying costs a duplicate attempt while wrongly
// giving up loses the message.
func interpret(resp *domain.Handoff) error {
	if resp == nil || resp.Payload == nil {
		return nil // accepted, nothing to report
	}
	content := strings.TrimSpace(string(resp.Payload.Data))
	if content == "" {
		return nil
	}

	var ack struct {
		Status    string `json:"status"`
		Permanent bool   `json:"permanent"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(content), &ack); err != nil {
		// A daemon that answered something unparseable has still probably sent the
		// message; treating that as a failure would duplicate it on retry.
		return nil
	}
	if ack.Status == "" || ack.Status == "ok" || ack.Status == "sent" {
		return nil
	}

	reason := ack.Error
	if reason == "" {
		reason = ack.Status
	}
	if ack.Permanent {
		return domain.PermanentDelivery(errors.New(reason))
	}
	return errors.New("delivery: ingress reported " + reason)
}

var _ domain.IngressTransport = (*DaemonTransport)(nil)
