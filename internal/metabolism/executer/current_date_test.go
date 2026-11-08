package executer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261107-planner-and-chat-agent-are-not-told-today-s: every step is told the
// organisation's date by the kernel, whatever the caller's context says.

func fixedCalendar(y int, m time.Month, d int) *domain.Calendar {
	return domain.NewCalendar(time.UTC, func() time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) })
}

func TestDAGExecutor_StampsTheCurrentDateOnEveryStep(t *testing.T) {
	cal := fixedCalendar(2026, 11, 7)
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
		{Query: "list the opportunities"},
		{Query: "keep those idle for the last two weeks", DependsOn: []int{0}},
	}}
	var mu sync.Mutex
	seen := map[int]string{}
	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		mu.Lock()
		seen[i] = h.Context[domain.HandoffKeyCurrentDate]
		mu.Unlock()
		// An agent that echoes its input context must not write the key back.
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")},
			Context: map[string]string{domain.HandoffKeyCurrentDate: "forged by the agent"}}, nil
	}
	initial := map[string]string{domain.HandoffKeyCurrentDate: "forged by the caller"}
	got, err := (&DAGExecutor{Calendar: cal}).Execute(t.Context(), plan, initial, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	want := cal.Block()
	for i := range plan.Steps {
		if seen[i] != want {
			t.Errorf("step %d was told %q, want the calendar block", i, seen[i])
		}
	}
	for k, v := range got {
		if v == "forged by the agent" {
			t.Errorf("an agent-echoed date merged into the plan context as %s", k)
		}
	}
}

// recordingCache keeps every key it is asked to store, in order.
type recordingCache struct {
	mu   sync.Mutex
	puts []string
}

func (c *recordingCache) Get(context.Context, string) (*domain.Handoff, bool, error) {
	return nil, false, nil
}

func (c *recordingCache) Put(_ context.Context, key string, _ *domain.Handoff, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, key)
	return nil
}

// A dependent step's cached answer to "the last two weeks" is keyed to the day it
// was computed for: the same inputs on another day are a different question.
func TestDAGExecutor_StepCacheIsScopedToTheDate(t *testing.T) {
	plan := func() *domain.ExecutionPlan {
		return &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
			{Query: "list the opportunities"},
			{Query: "keep those idle for the last two weeks", DependsOn: []int{0}},
		}}
	}
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("OPP-1, OPP-2")}}, nil
	}
	dependentKey := func(cal *domain.Calendar) string {
		c := &recordingCache{}
		if _, err := (&DAGExecutor{Calendar: cal, StepCache: c}).Execute(t.Context(), plan(), nil, stepFn); err != nil {
			t.Fatal(err)
		}
		if len(c.puts) != 2 {
			t.Fatalf("puts = %d, want 2", len(c.puts))
		}
		return c.puts[1] // the dependent step runs after its source
	}
	day1a, day1b, day2 := dependentKey(fixedCalendar(2026, 11, 7)), dependentKey(fixedCalendar(2026, 11, 7)), dependentKey(fixedCalendar(2026, 11, 8))
	if day1a != day1b {
		t.Error("the same day and inputs must reuse the cache key")
	}
	if day1a == day2 {
		t.Error("another day must not replay the cached answer")
	}
}

func TestDayScopedCacheKey(t *testing.T) {
	if dayScopedCacheKey("k", "") != "k" {
		t.Error("no date leaves the key unchanged")
	}
	if dayScopedCacheKey("k", "today: 2026-11-07") == dayScopedCacheKey("k", "today: 2026-11-08") {
		t.Error("different dates must give different keys")
	}
}
