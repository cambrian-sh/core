package telemetry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewBridge_NoExporterReturnsNoopObserver(t *testing.T) {
	cfg := config.TelemetryConfig{}
	obs := telemetry.NewBridge(cfg)
	if obs == nil {
		t.Fatal("NewBridge returned nil")
	}
	if _, ok := obs.(domain.NoopTelemetryObserver); !ok {
		t.Errorf("expected domain.NoopTelemetryObserver, got %T", obs)
	}
}

func TestNoopObserver_AllMethodsNoPanic(t *testing.T) {
	cfg := config.TelemetryConfig{}
	obs := telemetry.NewBridge(cfg)
	obs.OnTaskCompleted(domain.TaskEvent{})
	obs.OnSessionEvicted("agent-1")
	obs.OnConwipWait(100)
	obs.OnAuctionNoWinner("task-1")
	obs.OnSchemaMismatch("agent-1", "missing-field")
}

func TestBridge_OnTaskCompleted_BudgetOverrunIncrementsCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	cfg := config.TelemetryConfig{}
	obs := telemetry.NewBridgeWithProvider(cfg, mp)

	evt := domain.TaskEvent{
		TaskID:        "task-1",
		AgentID:       "agent-1",
		BudgetOverrun: true,
		TotalTokens:   1500,
	}
	obs.OnTaskCompleted(evt)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	counterName := "cambrian_budget_overrun_total"
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == counterName {
				got, ok := m.Data.(metricdata.Sum[int64])
				if !ok || !got.IsMonotonic {
					continue
				}
				if got.DataPoints[0].Value != 1 {
					t.Errorf("%s = %d, want 1", counterName, got.DataPoints[0].Value)
				}
				found = true
			}
		}
	}
	if !found {
		t.Errorf("counter %q not found in collected metrics", counterName)
	}
}

func TestBridge_OnTaskCompleted_FallbackModelUsedIncrementsCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	cfg := config.TelemetryConfig{}
	obs := telemetry.NewBridgeWithProvider(cfg, mp)

	evt := domain.TaskEvent{
		TaskID:            "task-2",
		AgentID:           "agent-2",
		FallbackModelUsed: true,
		ActualModelID:     "fallback-model",
	}
	obs.OnTaskCompleted(evt)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "cambrian_fallback_model_total" {
				got, ok := m.Data.(metricdata.Sum[int64])
				if !ok || !got.IsMonotonic {
					continue
				}
				if got.DataPoints[0].Value != 1 {
					t.Errorf("%s = %d, want 1", m.Name, got.DataPoints[0].Value)
				}
				attrs := got.DataPoints[0].Attributes
				if v, ok := attrs.Value(attribute.Key("actual_model_id")); !ok || v.AsString() != "fallback-model" {
					t.Errorf("actual_model_id attribute = %v, want fallback-model", v.AsString())
				}
				return
			}
		}
	}
	t.Error(`counter "cambrian_fallback_model_total" not found`)
}
