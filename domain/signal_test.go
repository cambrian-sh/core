package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Cycle 1 — WatchAction.TargetType field exists and round-trips through JSON.
func TestWatchAction_TargetTypeField(t *testing.T) {
	action := domain.WatchAction{
		Type:       "dispatch_agent",
		TargetType: "agent_id",
		Target:     "my-agent",
		Payload:    "{{price}}",
	}

	data, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got domain.WatchAction
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.TargetType != "agent_id" {
		t.Errorf("TargetType: want %q, got %q", "agent_id", got.TargetType)
	}
}

// Cycle 2 — WatchConfig new fields compile and round-trip through JSON.
func TestWatchConfig_NewFields(t *testing.T) {
	cfg := domain.WatchConfig{
		ID:                 "wc-1",
		ResponseMode:       "sync",
		DaemonParams:       map[string]any{"timeout_ms": float64(5000), "model": "gpt-4"},
		MaxConcurrentPlans: 2,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got domain.WatchConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ResponseMode != "sync" {
		t.Errorf("ResponseMode: want %q, got %q", "sync", got.ResponseMode)
	}
	if got.MaxConcurrentPlans != 2 {
		t.Errorf("MaxConcurrentPlans: want 2, got %d", got.MaxConcurrentPlans)
	}
	if v, ok := got.DaemonParams["timeout_ms"]; !ok || v != float64(5000) {
		t.Errorf("DaemonParams[timeout_ms]: want 5000, got %v", got.DaemonParams["timeout_ms"])
	}
}

// Cycle 3 — ConditionTypeAlways constant equals "always".
func TestConditionTypeAlways_Constant(t *testing.T) {
	if domain.ConditionTypeAlways != "always" {
		t.Errorf("ConditionTypeAlways: want %q, got %q", "always", domain.ConditionTypeAlways)
	}
}

// Cycle 4 — WatchTriggeredEvent.EventType() returns "watch.triggered" when ActionTarget is empty.
func TestWatchTriggeredEvent_DefaultEventType(t *testing.T) {
	evt := domain.WatchTriggeredEvent{
		WatchConfigID: "wc-1",
		StreamID:      "gold_tracker",
		SignalPayload: map[string]any{"price": 5100},
	}

	if got := evt.EventType(); got != "watch.triggered" {
		t.Errorf("EventType: want %q, got %q", "watch.triggered", got)
	}
}

// Cycle 5 — WatchTriggeredEvent.EventType() returns ActionTarget when set.
func TestWatchTriggeredEvent_ActionTargetOverridesEventType(t *testing.T) {
	evt := domain.WatchTriggeredEvent{
		WatchConfigID: "wc-2",
		StreamID:      "stream-x",
		ActionTarget:  "price.alert",
	}

	if got := evt.EventType(); got != "price.alert" {
		t.Errorf("EventType with ActionTarget: want %q, got %q", "price.alert", got)
	}
}

// Cycle 6 — WatchTriggeredEvent can be published via EventBus and routes to the correct subscriber.
func TestWatchTriggeredEvent_PublishesViaEventBus(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	var received domain.WatchTriggeredEvent
	bus.Subscribe(domain.EventTypeWatchTriggered, func(e domain.DomainEvent) {
		if wt, ok := e.(domain.WatchTriggeredEvent); ok {
			received = wt
		}
	})

	want := domain.WatchTriggeredEvent{
		WatchConfigID: "wc-3",
		StreamID:      "sensor-1",
		SignalPayload: map[string]any{"temp": float64(99)},
		ActionTarget:  "",
	}
	if err := bus.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if received.WatchConfigID != "wc-3" {
		t.Errorf("WatchConfigID: want %q, got %q", "wc-3", received.WatchConfigID)
	}
	if received.StreamID != "sensor-1" {
		t.Errorf("StreamID: want %q, got %q", "sensor-1", received.StreamID)
	}
}

// Compile-time assertion: WatchTriggeredEvent implements DomainEvent.
var _ domain.DomainEvent = domain.WatchTriggeredEvent{}
