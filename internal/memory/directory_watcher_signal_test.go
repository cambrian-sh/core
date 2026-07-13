package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// captureSignalReceiver records signals delivered to it.
type captureSignalReceiver struct {
	signals []domain.Signal
}

func (r *captureSignalReceiver) OnSignal(_ context.Context, sig domain.Signal) error {
	r.signals = append(r.signals, sig)
	return nil
}

// Cycle 52 — DirectoryWatcher.Start with a SignalReceiver sends Signal on file creation.
func TestDirectoryWatcher_SendsSignalOnFileCreate(t *testing.T) {
	dir := t.TempDir()
	receiver := &captureSignalReceiver{}

	dw := memory.NewDirectoryWatcher(dir, nil) // no enqueue function
	dw.SignalReceiver = receiver

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dw.Start(ctx)

	// Give the watcher time to initialise before writing the file.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "test.md"), []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(receiver.signals) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if len(receiver.signals) == 0 {
		t.Fatal("expected at least one Signal to be delivered to SignalReceiver")
	}

	sig := receiver.signals[0]
	if sig.StreamID != dir {
		t.Errorf("expected StreamID=%q, got %q", dir, sig.StreamID)
	}
	if _, ok := sig.Payload["path"]; !ok {
		t.Error("expected 'path' field in Signal Payload")
	}
	if _, ok := sig.Payload["extension"]; !ok {
		t.Error("expected 'extension' field in Signal Payload")
	}
}

// Cycle 53 — DirectoryWatcher.Start with nil SignalReceiver still uses enqueue
// (backward compatibility when SignalReceiver is not set).
func TestDirectoryWatcher_NilSignalReceiver_UsesEnqueue(t *testing.T) {
	dir := t.TempDir()

	var enqueued []domain.ExternalDocument
	dw := memory.NewDirectoryWatcher(dir, func(doc domain.ExternalDocument) bool {
		enqueued = append(enqueued, doc)
		return true
	})
	// SignalReceiver intentionally not set

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dw.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("content"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(enqueued) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if len(enqueued) == 0 {
		t.Fatal("expected enqueue to be called when SignalReceiver is nil")
	}
}
