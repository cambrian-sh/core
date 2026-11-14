package app

import (
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// streamRegistryHolder is the late-bound contract-0074 stream registry
// (KernelServices.RegisterStreamRegistry). The operator service is built before
// plugins Build, so it holds this holder; a plugin's registration lands in it
// and every ListWatches after that reads the real liveness. Until then (and
// forever on a kernel without one) it answers exactly what an unwired kernel
// answered: StreamUnknown and refcount -1 — "cannot tell", never a guess.
type streamRegistryHolder struct {
	v atomic.Pointer[domain.StreamRegistry]
}

func (h *streamRegistryHolder) set(r domain.StreamRegistry) {
	if r == nil {
		return
	}
	h.v.Store(&r)
}

func (h *streamRegistryHolder) get() domain.StreamRegistry {
	if p := h.v.Load(); p != nil {
		return *p
	}
	return nil
}

// StreamState implements domain.StreamRegistry.
func (h *streamRegistryHolder) StreamState(streamID string) string {
	if r := h.get(); r != nil {
		return r.StreamState(streamID)
	}
	return domain.StreamUnknown
}

// StreamRefcount implements domain.StreamRegistry.
func (h *streamRegistryHolder) StreamRefcount(streamID string) int {
	if r := h.get(); r != nil {
		return r.StreamRefcount(streamID)
	}
	return -1
}

var _ domain.StreamRegistry = (*streamRegistryHolder)(nil)
