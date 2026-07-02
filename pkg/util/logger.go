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
	File            *os.File
	OriginalStdout  *os.File
	OriginalStderr  *os.File
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
		slog.SetDefault(slog.New(handler))
		return &LoggerResult{OriginalStderr: os.Stderr}, nil
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
	// Redirect structured logger.
	slog.SetDefault(slog.New(slog.NewTextHandler(f, opts)))
	// Redirect direct stdout/stderr writes from any library.
	os.Stdout = f
	os.Stderr = f

	return &LoggerResult{File: f, OriginalStdout: originalStdout, OriginalStderr: originalStderr}, nil
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
