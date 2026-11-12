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

// T-20261109-per-step-deadline-step-timeout-floor-ms-180-s: the composed runtime's
// tool budget is config, not the SDK's fixed 12.
func TestWorkersEnv(t *testing.T) {
	if got := workersEnv(config.WorkersConfig{}); got != nil {
		t.Errorf("zero config sets nothing: %v", got)
	}
	got := workersEnv(config.WorkersConfig{EvidenceReprompt: true, MaxToolRounds: 500})
	if len(got) != 2 || got[0] != "CAMBRIAN_EVIDENCE_REPROMPT=1" || got[1] != "CAMBRIAN_COMPOSED_MAX_TOOL_ROUNDS=500" {
		t.Errorf("on: %v", got)
	}
}
