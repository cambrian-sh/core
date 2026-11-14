package app

import (
	"reflect"
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

// The boot log used to say "the runtime will export traces/metrics as configured" for
// otlp_endpoint / prometheus_port, for which no exporter exists.
func TestUnbuiltTelemetryExporters(t *testing.T) {
	t.Parallel()
	if got := unbuiltTelemetryExporters(config.TelemetryConfig{EnableStdoutExporter: true}); len(got) != 0 {
		t.Errorf("stdout-only must report nothing, got %v", got)
	}
	got := unbuiltTelemetryExporters(config.TelemetryConfig{OTLPEndpoint: "http://x:4317", PrometheusPort: 9464})
	if want := []string{"otlp_endpoint", "prometheus_port"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
