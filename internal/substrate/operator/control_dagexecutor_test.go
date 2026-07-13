package operator_test

import (
	"testing"

	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// A live DAGExecutor satisfies operator.ExecutionControls and can be registered
// in the hub for operator steering. ADR-0047 0047-18.
var _ operator.ExecutionControls = (*executer.DAGExecutor)(nil)

func TestControlHub_RegistersAndSteersDAGExecutor(t *testing.T) {
	hub := operator.NewExecutionControlHub()
	d := &executer.DAGExecutor{}

	hub.Register("s1", d)
	c, ok := hub.Lookup("s1")
	if !ok || c == nil {
		t.Fatal("expected the DAGExecutor registered under s1")
	}
	// Steering the live execution via the hub handle must not panic (coordinator
	// pause is safe with no active Execute).
	c.Pause()
	c.Resume()

	hub.Deregister("s1")
	if _, ok := hub.Lookup("s1"); ok {
		t.Fatal("expected s1 deregistered after completion")
	}
}
