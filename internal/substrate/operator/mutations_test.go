package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// TriggerConsolidation publishes a MemoryPressureEvent via EventBusEffects, and
// a retried command_id applies the effect exactly once.
func TestTriggerConsolidation_PublishesAndIsIdempotent(t *testing.T) {
	svc, _, audit, _ := newCommandService()
	bus := domain.NewInMemoryEventBus()
	var pressures int
	bus.Subscribe(domain.EventTypeMemoryPressure, func(domain.DomainEvent) { pressures++ })
	svc.SetCommandEffects(operator.EventBusEffects{Bus: bus})

	req := &pb.TriggerConsolidationRequest{CommandId: "cons-1", Reason: "manual sweep", Scope: "session-x"}
	if _, err := svc.TriggerConsolidation(opCtx(), req); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	ack, err := svc.TriggerConsolidation(opCtx(), req) // retry
	if err != nil || !ack.GetDeduped() {
		t.Fatalf("retry should dedup, got ack=%+v err=%v", ack, err)
	}
	if pressures != 1 {
		t.Fatalf("expected exactly one MemoryPressureEvent, got %d", pressures)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 1 || rows[0].ActionType != "trigger_consolidation" {
		t.Fatalf("expected one trigger_consolidation audit row, got %+v", rows)
	}
}

// The remaining mutations audit through the uniform command path (NoopEffects).
func TestRemainingMutations_AuditUniformly(t *testing.T) {
	svc, _, audit, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})

	if _, err := svc.TagMemory(opCtx(), &pb.TagMemoryRequest{CommandId: "t1", Reason: "classify", DocId: "d1", Tag: "secret", Add: true}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, err := svc.SetScope(opCtx(), &pb.SetScopeRequest{CommandId: "s1", Reason: "widen", AgentId: "a1", AnyOfTags: []string{"kb"}}); err != nil {
		t.Fatalf("scope: %v", err)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 2 {
		t.Fatalf("expected 2 audit rows, got %d", len(rows))
	}
}

// SetRuntimeConfig routes the param map to the effect, applies exactly once per
// command_id, and audits as set_runtime_config (ADR-0054 tuning seam).
func TestSetRuntimeConfig_AppliesAndIsIdempotent(t *testing.T) {
	svc, _, audit, _ := newCommandService()
	var got map[string]float64
	var calls int
	svc.SetCommandEffects(operator.CommandEffectsFuncs{
		SetRuntimeConfigFn: func(_ context.Context, params map[string]float64) error {
			calls++
			got = params
			return nil
		},
	})

	req := &pb.SetRuntimeConfigRequest{
		CommandId: "cfg-1", Reason: "blend sweep step",
		Params: map[string]float64{"blend_weight_recency": 0.2, "blend_weight_pagerank": 0.0},
	}
	if _, err := svc.SetRuntimeConfig(opCtx(), req); err != nil {
		t.Fatalf("set: %v", err)
	}
	ack, err := svc.SetRuntimeConfig(opCtx(), req) // retry same command_id
	if err != nil || !ack.GetDeduped() {
		t.Fatalf("retry should dedup, got ack=%+v err=%v", ack, err)
	}
	if calls != 1 {
		t.Fatalf("effect must apply exactly once, got %d", calls)
	}
	if got["blend_weight_recency"] != 0.2 {
		t.Fatalf("params not routed to the effect, got %+v", got)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 1 || rows[0].ActionType != "set_runtime_config" {
		t.Fatalf("expected one set_runtime_config audit row, got %+v", rows)
	}
}

// CommandEffectsFuncs routes to bound functions and returns Unimplemented for
// nil hooks (ADR-0047 0047-16).
func TestCommandEffectsFuncs_RoutesAndUnimplemented(t *testing.T) {
	var scopedAgent string
	eff := operator.CommandEffectsFuncs{
		SetScopeFn: func(_ context.Context, agentID string, _, _, _ []string) error {
			scopedAgent = agentID
			return nil
		},
		// TagMemoryFn left nil → Unimplemented (gated on 0047-20).
	}

	if err := eff.SetScope(context.Background(), "agent-7", nil, []string{"kb"}, nil); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	if scopedAgent != "agent-7" {
		t.Fatalf("expected SetScopeFn called for agent-7, got %q", scopedAgent)
	}
	if err := eff.TagMemory(context.Background(), "d1", "secret", true); status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for nil TagMemoryFn, got %v", err)
	}
}
