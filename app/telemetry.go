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

func initTelemetry(cfg *config.Config) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	// otlp_endpoint and prometheus_port were retired in schema v3 (owner decision
	// 2026-11-14, ENG-203): no exporter for them was ever built. The stdout exporter
	// is the only one this build has, so it alone switches telemetry on.
	if !cfg.Telemetry.EnableStdoutExporter {
		return nil, nil // ADR-0057 (D11): telemetry off by default — stay silent.
	}
	// ADR-0057 (D11): announce telemetry activation for transparency (only when enabled).
	slog.Info("telemetry enabled", "stdout_exporter", cfg.Telemetry.EnableStdoutExporter)
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
