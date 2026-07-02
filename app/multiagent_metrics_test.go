//go:build integration

package app

import (
	"math"
	"testing"
)

func TestRoutingAccuracy_AllMatched(t *testing.T) {
	results := []stepRoutingResult{
		{stepIndex: 0, expected: "code_generation", actual: "code_generation", matched: true},
		{stepIndex: 1, expected: "code_execution", actual: "code_execution", matched: true},
	}
	if got := routingAccuracy(results); got != 1.0 {
		t.Errorf("expected 1.0, got %v", got)
	}
}

func TestRoutingAccuracy_PartialMatch(t *testing.T) {
	results := []stepRoutingResult{
		{matched: true},
		{matched: false},
		{matched: true},
		{matched: true},
	}
	got := routingAccuracy(results)
	if math.Abs(got-0.75) > 1e-9 {
		t.Errorf("expected 0.75, got %v", got)
	}
}

func TestRoutingAccuracy_EmptyIsZero(t *testing.T) {
	if got := routingAccuracy(nil); got != 0 {
		t.Errorf("expected 0 for empty, got %v", got)
	}
}

func TestPlannerDelta(t *testing.T) {
	if got := plannerDelta(4.0, 3.5); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("expected 0.5, got %v", got)
	}
	if got := plannerDelta(3.0, 4.0); math.Abs(got-(-1.0)) > 1e-9 {
		t.Errorf("expected -1.0, got %v", got)
	}
}

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("hello", 10); got != "hello" {
		t.Errorf("expected full string, got %q", got)
	}
	if got := truncateStr("hello world", 5); got != "hello" {
		t.Errorf("expected truncated, got %q", got)
	}
}
