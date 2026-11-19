package app

import (
	"io"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/runtrace"
)

// wireRunTrace opens the decision-level run trace (ADR-0145 D10) when
// execution.trace.enabled: one JSONL file per kernel process in execution.trace.dir,
// fed by the LLM provider (every model call), the DAG executor (every step output)
// and ExecuteTool (every kernel-executed tool call). Spawned agents are pointed at
// the same directory by the agent spawner (kernel.NewMetabolismStack). The writer
// flushes every span, so a crash keeps the last spans; Kernel.Shutdown closes the
// file (runTraceCloser, ENG-2102) so an in-process kernel releases it. Off, or a directory that cannot be created, returns nil and nothing is traced
// (logged; a trace must never stop the kernel).
func wireRunTrace(c config.TraceConfig) domain.RunTracer {
	if !c.Enabled {
		return nil
	}
	w, err := runtrace.Open(c.Dir, "cambrian-kernel", c.ContentCap())
	if err != nil {
		slog.Error("run trace: cannot open, tracing is OFF", "dir", c.Dir, "err", err)
		return nil
	}
	slog.Info("run trace: on (ADR-0145 D10)", "path", w.Path(), "max_content_chars", c.ContentCap())
	return w
}

// runTraceCloser is the handle Kernel.Shutdown closes: the tracer wireRunTrace
// opened, or nil when tracing is off (ENG-2102).
func runTraceCloser(tr domain.RunTracer) io.Closer {
	if c, ok := tr.(io.Closer); ok && tr != nil {
		return c
	}
	return nil
}
