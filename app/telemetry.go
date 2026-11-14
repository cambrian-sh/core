package app

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/cambrian-sh/core/internal/config"
)

// defaultMetricsExportInterval is the period used when
// telemetry.metrics_export_interval_seconds is unset or not positive. It equals the
// shipped default (DefaultConfig: 10) and the interval that was hardcoded before the
// key was threaded here, so an unset key behaves exactly as before.
const defaultMetricsExportInterval = 10 * time.Second

// metricsExportInterval is the PeriodicReader interval for telemetry
// (telemetry.metrics_export_interval_seconds). The key was parsed, defaulted and
// shipped in config.example.json but read by nothing (AGENTS.md §3.4;
// T-20261113-eight-config-fields-are-defaulted-some).
func metricsExportInterval(t config.TelemetryConfig) time.Duration {
	if t.MetricsExportIntervalSeconds <= 0 {
		return defaultMetricsExportInterval
	}
	return time.Duration(t.MetricsExportIntervalSeconds) * time.Second
}

// unbuiltTelemetryExporters names the telemetry keys that are set but have no
// exporter in this build. PREM-0019 D3 promises Prometheus pull on prometheus_port
// and OTLP push to otlp_endpoint, but core ships only the stdout metric exporter
// (go.mod has no OTLP or Prometheus exporter), so these keys switch the meter
// provider on and nothing is exported through them
// (T-20261031-telemetry-otlp-endpoint-and-prometheus-port-are). Whether to build the
// exporters or reject the keys is an owner decision; until then the boot log says so
// instead of claiming export.
func unbuiltTelemetryExporters(t config.TelemetryConfig) []string {
	var out []string
	if t.OTLPEndpoint != "" {
		out = append(out, "otlp_endpoint")
	}
	if t.PrometheusPort != 0 {
		out = append(out, "prometheus_port")
	}
	return out
}

func initTelemetry(cfg *config.Config) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	if cfg.Telemetry.OTLPEndpoint == "" && cfg.Telemetry.PrometheusPort == 0 && !cfg.Telemetry.EnableStdoutExporter {
		return nil, nil // ADR-0057 (D11): telemetry off by default — stay silent.
	}
	// ADR-0057 (D11): announce telemetry activation for transparency (only when enabled).
	slog.Info("telemetry enabled",
		"otlp_endpoint", cfg.Telemetry.OTLPEndpoint,
		"prometheus_port", cfg.Telemetry.PrometheusPort,
		"stdout_exporter", cfg.Telemetry.EnableStdoutExporter)
	if unbuilt := unbuiltTelemetryExporters(cfg.Telemetry); len(unbuilt) > 0 {
		slog.Warn("telemetry: no exporter is built for these keys; nothing is exported through them "+
			"(only enable_stdout_exporter has an exporter in this build)",
			"keys", unbuilt, "ticket", "T-20261031-telemetry-otlp-endpoint-and-prometheus-port-are")
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.Telemetry.TraceSamplingRate)),
	)
	otel.SetTracerProvider(tp)

	var mpOpts []sdkmetric.Option
	if cfg.Telemetry.EnableStdoutExporter {
		stdoutExp, err := stdoutmetric.New(stdoutmetric.WithPrettyPrint())
		if err != nil {
			slog.Warn("telemetry: failed to create stdout metric exporter", "err", err)
		} else {
			mpOpts = append(mpOpts, sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(stdoutExp, sdkmetric.WithInterval(metricsExportInterval(cfg.Telemetry))),
			))
		}
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)
	return tp, mp
}
