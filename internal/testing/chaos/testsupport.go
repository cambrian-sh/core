package chaos

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// LogCapture records every slog record emitted through the default logger while
// it is installed. The kernel's recovery paths are "warn and continue" — the warn
// IS the contract (a swallowed failure with no line of evidence is the defect
// class AGENTS.md §3.2 names), so a chaos scenario asserts on it.
type LogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

// CaptureLogs installs a capturing default logger for the duration of t and
// restores the previous default on cleanup. Not safe with t.Parallel: the default
// logger is process-global.
func CaptureLogs(t *testing.T) *LogCapture {
	t.Helper()
	c := &LogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func (c *LogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *LogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}

func (c *LogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *LogCapture) WithGroup(string) slog.Handler      { return c }

// Count returns how many records at or above level contain substr in their
// message.
func (c *LogCapture) Count(level slog.Level, substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.records {
		if r.Level >= level && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// WaitUntil polls cond every 10ms until it holds or deadline elapses. It is the
// deadline-bounded replacement for a bare sleep in a timing-sensitive scenario.
func WaitUntil(t *testing.T, deadline time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", deadline, what)
}
