package reactive

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// NoOpSignalReceiver implements domain.SignalReceiver as a stub.
// It logs every received signal at DEBUG level and discards it.
// ADR-0032's ReactiveEngine will replace this in the composition root.
type NoOpSignalReceiver struct{}

// OnSignal implements domain.SignalReceiver. It logs the signal and returns nil.
func (r *NoOpSignalReceiver) OnSignal(_ context.Context, sig domain.Signal) error {
	slog.Debug("SignalReceiver: signal received (no-op stub)",
		"stream_id", sig.StreamID,
		"from_agent", sig.FromAgent,
		"payload_keys", len(sig.Payload),
	)
	return nil
}

var _ domain.SignalReceiver = (*NoOpSignalReceiver)(nil)
