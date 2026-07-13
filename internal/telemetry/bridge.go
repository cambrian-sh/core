package telemetry

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Bridge implements domain.TelemetryObserver and translates domain signals
// into OTel metrics and span events. Safe for concurrent use.
type Bridge struct {
	budgetOverrunCounter metric.Int64Counter
	fallbackCounter      metric.Int64Counter
	sessionLeakCounter   metric.Int64Counter
	conwipWaitHist       metric.Int64Histogram
	auctionNoWinner      metric.Int64Counter
	schemaMismatch       metric.Int64Counter
}

// NewBridge constructs a Bridge from TelemetryConfig using a default MeterProvider.
// Returns NoopTelemetryObserver when no exporter is configured.
func NewBridge(cfg config.TelemetryConfig) domain.TelemetryObserver {
	if cfg.OTLPEndpoint == "" && cfg.PrometheusPort == 0 && !cfg.EnableStdoutExporter {
		slog.Warn("telemetry: no exporter configured, all metrics dropped",
			"hint", "set otlp_endpoint or prometheus_port or enable_stdout_exporter")
		return domain.NoopTelemetryObserver{}
	}
	mp := otel.GetMeterProvider()
	return NewBridgeWithProvider(cfg, mp)
}

// NewBridgeWithProvider constructs a Bridge with the given MeterProvider.
// For testing with in-memory readers.
func NewBridgeWithProvider(_ config.TelemetryConfig, mp metric.MeterProvider) domain.TelemetryObserver {
	meter := mp.Meter("cambrian-core")
	b := &Bridge{}
	b.budgetOverrunCounter, _ = meter.Int64Counter("cambrian_budget_overrun_total",
		metric.WithDescription("Number of step completions where token budget was exceeded"))
	b.fallbackCounter, _ = meter.Int64Counter("cambrian_fallback_model_total",
		metric.WithDescription("Number of step completions where a fallback model was used"))
	b.sessionLeakCounter, _ = meter.Int64Counter("cambrian_session_eviction_total",
		metric.WithDescription("Number of expired session tokens evicted"))
	b.conwipWaitHist, _ = meter.Int64Histogram("cambrian_conwip_wait_ms",
		metric.WithDescription("CONWIP semaphore wait time in milliseconds"))
	b.auctionNoWinner, _ = meter.Int64Counter("cambrian_auction_no_winner_total",
		metric.WithDescription("Number of auctions where no winner was found"))
	b.schemaMismatch, _ = meter.Int64Counter("cambrian_schema_mismatch_total",
		metric.WithDescription("Number of schema mismatches detected in handoff mapping"))
	return b
}

func (b *Bridge) OnTaskCompleted(evt domain.TaskEvent) {
	ctx := context.Background()
	if evt.BudgetOverrun {
		b.budgetOverrunCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("agent_id", evt.AgentID),
		))
	}
	if evt.FallbackModelUsed {
		b.fallbackCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("actual_model_id", evt.ActualModelID),
		))
	}
}

func (b *Bridge) OnSessionEvicted(agentID string) {
	b.sessionLeakCounter.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("agent_id", agentID)))
}

func (b *Bridge) OnConwipWait(durationMs int64) {
	b.conwipWaitHist.Record(context.Background(), durationMs)
}

func (b *Bridge) OnAuctionNoWinner(taskID string) {
	b.auctionNoWinner.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("task_id", taskID)))
}

func (b *Bridge) OnSchemaMismatch(agentID, kind string) {
	b.schemaMismatch.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("agent_id", agentID),
			attribute.String("mismatch_type", kind),
		))
}

func (b *Bridge) OnPlanCompleted(_ domain.PlanEvent)     {}
func (b *Bridge) OnRetrievalCompleted(_ domain.RetrievalSession) {}
func (b *Bridge) OnContradictionResolved(_ domain.ContradictionResolution) {}
