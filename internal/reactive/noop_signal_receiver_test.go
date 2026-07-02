package reactive_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/reactive"
)

// Cycle 48 — NoOpSignalReceiver.OnSignal returns nil (no error).
func TestNoOpSignalReceiver_OnSignal_ReturnsNil(t *testing.T) {
	r := &reactive.NoOpSignalReceiver{}
	err := r.OnSignal(context.Background(), domain.Signal{
		StreamID:  "data/inbox/",
		FromAgent: "directory_watcher",
		Payload:   map[string]any{"path": "/data/inbox/report.md", "extension": "md"},
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

// Cycle 49 — NoOpSignalReceiver implements domain.SignalReceiver interface.
func TestNoOpSignalReceiver_ImplementsInterface(t *testing.T) {
	var _ domain.SignalReceiver = &reactive.NoOpSignalReceiver{}
}

// Cycle 50 — domain.Signal carries expected fields.
func TestSignal_FieldsAccessible(t *testing.T) {
	sig := domain.Signal{
		StreamID:  "data/inbox/",
		FromAgent: "directory_watcher",
		Payload: map[string]any{
			"path":      "/data/inbox/report.md",
			"extension": "md",
			"mime_type": "text/plain",
		},
		Timestamp: time.Now(),
	}
	if sig.StreamID == "" {
		t.Error("StreamID should not be empty")
	}
	if _, ok := sig.Payload["path"]; !ok {
		t.Error("expected 'path' in Payload")
	}
	if _, ok := sig.Payload["extension"]; !ok {
		t.Error("expected 'extension' in Payload")
	}
}

// Cycle 51 — domain.WatchConfig and WatchSource structs are accessible.
func TestWatchConfig_FieldsAccessible(t *testing.T) {
	cfg := domain.WatchConfig{
		ID:            "default-inbox-ingest",
		Source:        domain.WatchSource{Type: "filesystem", StreamID: "data/inbox/"},
		Condition:     "true",
		ConditionType: "deterministic",
		Action:        domain.WatchAction{Type: "ingest"},
	}
	if cfg.ID == "" {
		t.Error("WatchConfig ID should not be empty")
	}
	if cfg.Source.StreamID == "" {
		t.Error("WatchSource StreamID should not be empty")
	}
}
