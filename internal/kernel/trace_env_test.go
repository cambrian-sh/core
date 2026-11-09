package kernel

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func TestTraceEnv(t *testing.T) {
	if got := traceEnv(config.TraceConfig{}); got != nil {
		t.Errorf("off: %v", got)
	}
	got := traceEnv(config.TraceConfig{Enabled: true, Dir: "/tmp/tr"})
	if len(got) != 2 || got[0] != "CAMBRIAN_TRACE_DIR=/tmp/tr" || got[1] != "CAMBRIAN_TRACE_MAX_CHARS=20000" {
		t.Errorf("on: %v", got)
	}
}
