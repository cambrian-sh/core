package network

import (
	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// protoToHandoff converts a proto Handoff to its domain equivalent. Nil-safe.
// The observer is called when schema validation detects a mismatch.
func protoToHandoff(h *pb.Handoff, obs domain.TelemetryObserver) *domain.Handoff {
	if h == nil {
		return nil
	}
	if h.Id == "" || h.FromAgent == "" {
		if obs != nil {
			obs.OnSchemaMismatch(h.ToAgent, "missing_required_field")
		}
	}
	d := &domain.Handoff{
		ID:            h.Id,
		FromAgent:     h.FromAgent,
		ToAgent:       h.ToAgent,
		Confidence:    h.Confidence,
		Uncertainties: h.Uncertainties,
		Context:       h.Metadata, // ADR-0022: proto context → metadata; domain keeps Context name
	}
	if h.Payload != nil {
		d.Payload = &domain.Payload{
			ID:       h.Payload.Id,
			Type:     h.Payload.Type,
			Data:     h.Payload.Data,
			Metadata: h.Payload.Metadata,
		}
	}
	for _, ref := range h.WorkingMemory {
		d.WorkingMemory = append(d.WorkingMemory, domain.ContextRef{
			CID:        domain.CID(ref.Cid),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return d
}

// handoffToProto converts a domain Handoff back to its proto equivalent. Nil-safe.
func handoffToProto(d *domain.Handoff) *pb.Handoff {
	if d == nil {
		return nil
	}
	h := &pb.Handoff{
		Id:            d.ID,
		FromAgent:     d.FromAgent,
		ToAgent:       d.ToAgent,
		Confidence:    d.Confidence,
		Uncertainties: d.Uncertainties,
		Metadata:      d.Context, // ADR-0022: domain Context → proto metadata
	}
	if d.Payload != nil {
		h.Payload = &pb.Object{
			Id:       d.Payload.ID,
			Type:     d.Payload.Type,
			Data:     d.Payload.Data,
			Metadata: d.Payload.Metadata,
		}
	}
	for _, ref := range d.WorkingMemory {
		h.WorkingMemory = append(h.WorkingMemory, &pb.ContextRef{
			Cid:        string(ref.CID),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return h
}

