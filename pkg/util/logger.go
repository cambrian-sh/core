package util

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
)

// InitLogger initializes the global structured logger using slog.
// LogMode controls where logs are written.
type LogMode int

const (
	// LogModeHeadless writes logs to stdout (default behaviour).
	LogModeHeadless LogMode = iota
	// LogModeTUI redirects all logs to a file and suppresses stdout/stderr
	// so Bubble Tea has exclusive control of the terminal.
	LogModeTUI
)

// LoggerResult holds the opened log file and the original stdout/stderr handles.
type LoggerResult struct {
	File           *os.File
	OriginalStdout *os.File
	OriginalStderr *os.File
	// Ring is the in-process retention window. Every record the kernel logs is
	// also kept here, so the process can answer "what happened an hour ago"
	// without something outside it having been attached at the time.
	Ring *LogRing
}

// InitLogger sets up the global logger. In TUI mode it redirects everything
// to a log file inside dataDir; in headless mode it keeps stdout logging.
// The caller is responsible for closing the returned file (if non-nil).
func InitLogger(mode LogMode, dataDir string) (*LoggerResult, error) {
	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}

	if mode == LogModeHeadless {
		// Choose a handler that fits the sink: a human-readable text handler when
		// stdout is an interactive terminal (so a developer can scan what happened),
		// and structured JSON when it is piped/redirected (so log aggregators in a
		// container or CI keep machine-parseable output). No extra dependency — the
		// character-device bit on the stat is portable across Windows and POSIX.
		var handler slog.Handler
		if isTerminal(os.Stdout) {
			handler = slog.NewTextHandler(os.Stdout, opts)
		} else {
			handler = slog.NewJSONHandler(os.Stdout, opts)
		}
		// Retain as well as print. A decorator rather than a change at every call
		// site, so it captures everything already being logged — including the
		// agent output relayed through the agent manager.
		ring := NewLogRing(DefaultLogRingCapacity)
		SetDefaultLogRing(ring)
		slog.SetDefault(slog.New(NewLogRingHandler(handler, ring, slog.LevelDebug)))
		return &LoggerResult{OriginalStderr: os.Stderr, Ring: ring}, nil
	}

	// TUI mode: write to file, suppress stdout/stderr.
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logPath := filepath.Join(dataDir, "cambrian.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", logPath, err)
	}

	// Capture the real terminal handles BEFORE we redirect.
	originalStdout := os.Stdout
	originalStderr := os.Stderr

	// Redirect standard-library log package.
	log.SetOutput(f)
	// Redirect structured logger — still retained, because a TUI hides the very
	// output an operator would otherwise scroll back through.
	ring := NewLogRing(DefaultLogRingCapacity)
	SetDefaultLogRing(ring)
	slog.SetDefault(slog.New(NewLogRingHandler(slog.NewTextHandler(f, opts), ring, slog.LevelDebug)))
	// Redirect direct stdout/stderr writes from any library.
	os.Stdout = f
	os.Stderr = f

	return &LoggerResult{
		File:           f,
		OriginalStdout: originalStdout,
		OriginalStderr: originalStderr,
		Ring:           ring,
	}, nil
}

// isTerminal reports whether f is an interactive character device (a TTY/console)
// rather than a pipe or regular file. Uses the os.ModeCharDevice bit so it works
// on both Windows consoles and POSIX terminals without a platform-specific syscall.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
