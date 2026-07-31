package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// captureSignalReceiver records signals delivered to it.
//
// QA-01: guarded because the DirectoryWatcher delivers from its own goroutine
// while the test polls from another. Like captureAllStore, the race the detector
// found is in this fake and not in the watcher — the watcher does the ordinary
// thing and the fake was simply not safe to share.
type captureSignalReceiver struct {
	mu      sync.Mutex
	signals []domain.Signal
}

func (r *captureSignalReceiver) OnSignal(_ context.Context, sig domain.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, sig)
	return nil
}

// count and at read under the lock, so the polling loop and the assertions below
// never touch the slice while the watcher is appending to it.
func (r *captureSignalReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.signals)
}

func (r *captureSignalReceiver) at(i int) domain.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signals[i]
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
	for time.Now().Before(deadline) && receiver.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if receiver.count() == 0 {
		t.Fatal("expected at least one Signal to be delivered to SignalReceiver")
	}

	sig := receiver.at(0)
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

	// Guarded for the same reason as the fakes above: the enqueue callback runs on
	// the watcher's goroutine and the poll below runs on the test's.
	var mu sync.Mutex
	var enqueued []domain.ExternalDocument
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(enqueued)
	}
	dw := memory.NewDirectoryWatcher(dir, func(doc domain.ExternalDocument) bool {
		mu.Lock()
		defer mu.Unlock()
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
	for time.Now().Before(deadline) && count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if count() == 0 {
		t.Fatal("expected enqueue to be called when SignalReceiver is nil")
	}
}
