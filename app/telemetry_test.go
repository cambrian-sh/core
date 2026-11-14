package app

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// REGRESSION (T-20261113-eight-config-fields-are-defaulted-some): the stdout reader's
// interval was hardcoded to 10 s and telemetry.metrics_export_interval_seconds was read
// by nothing. Unset/zero keeps the old 10 s.
func TestMetricsExportInterval_ReadsTheKey(t *testing.T) {
	t.Parallel()
	cases := map[int]time.Duration{0: 10 * time.Second, -5: 10 * time.Second, 10: 10 * time.Second, 30: 30 * time.Second}
	for in, want := range cases {
		if got := metricsExportInterval(config.TelemetryConfig{MetricsExportIntervalSeconds: in}); got != want {
			t.Errorf("metrics_export_interval_seconds=%d: got %v, want %v", in, got, want)
		}
	}
	if got := metricsExportInterval(config.DefaultConfig().Telemetry); got != defaultMetricsExportInterval {
		t.Errorf("shipped default must keep the pre-change 10 s, got %v", got)
	}
}
