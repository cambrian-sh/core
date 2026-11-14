// Package agentwire maps a domain.Handoff to and from its proto form at the
// kernel → agent gRPC boundary. It is the ONE mapping both agent call paths use —
// agentplane.Transport (production) and agentmgr.AgentManager (the plugin
// CallAgent/CallDaemon path) — so a Handoff field added here reaches agents on
// both, instead of silently vanishing on one copy
// (T-20261113-handoff-proto-converters-are-duplicated-in).
package agentwire

import (
	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ToProto converts a domain Handoff to proto for gRPC calls.
// ADR-0022 Phase 3: WorkingMemory is copied into proto working_memory so
// cognitive agents receive prior-step ContextRefs via the Python SDK.
func ToProto(d *domain.Handoff) *pb.Handoff {
	if d == nil {
		return nil
	}
	h := &pb.Handoff{
		Id:            d.ID,
		FromAgent:     d.FromAgent,
		ToAgent:       d.ToAgent,
		Confidence:    d.Confidence,
		Uncertainties: d.Uncertainties,
		Metadata:      d.Context,
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

// FromProto converts a proto Handoff to domain for internal use.
// ADR-0022 Phase 3: working_memory is copied back into domain WorkingMemory.
func FromProto(h *pb.Handoff) *domain.Handoff {
	if h == nil {
		return nil
	}
	d := &domain.Handoff{
		ID:            h.Id,
		FromAgent:     h.FromAgent,
		ToAgent:       h.ToAgent,
		Confidence:    h.Confidence,
		Uncertainties: h.Uncertainties,
		Context:       h.GetMetadata(),
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
