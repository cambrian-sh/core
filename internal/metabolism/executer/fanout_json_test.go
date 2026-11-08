package executer_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// o3-ar-reminders #1 (run R-20261107T182608Z-default, kernel-U3 21:42:28): the read
// step answered with a JSON object holding two arrays, then the requested item array
// on its last line. The read-only lookup fan-out over it was split into its 26 JSON
// lines ("For the customer \"contact_email\": …") and ran into the caller's deadline
// (T-20261107-read-only-fan-out-over-an-unrecognised-json).
const o3Object = "{\n  \"customers_60plus_overdue\": [\n    {\n      \"customer_name\": \"Wychwood Clinic\",\n      \"overdue_amount_60plus\": 20092.11\n    },\n" +
	"    {\n      \"customer_name\": \"Marlowe Park Pharmacy\",\n      \"overdue_amount_60plus\": 25602.43\n    }\n  ],\n" +
	"  \"follow_up_contacts\": [\n    {\n      \"customer_name\": \"Wychwood Clinic\",\n      \"account_id\": \"CUS-002\"\n    }\n  ]\n}"

func runReadFanOut(t *testing.T, source string) []string {
	t.Helper()
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "aging report", RequiredEffects: []string{"read"}},
		{Query: "For the customer {item}, look up the account", DependsOn: []int{0}, FanOutOver: ptr(0), RequiredEffects: []string{"read"}},
	}}
	var mu sync.Mutex
	var lookups []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		out := "ok"
		if strings.HasPrefix(q, "aging report") {
			out = source
		} else {
			mu.Lock()
			lookups = append(lookups, q)
			mu.Unlock()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	if _, err := (&executer.DAGExecutor{MaxFanOutWidth: 50, FanOutConcurrency: 4}).Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return lookups
}

func TestDAGExecutor_FanOut_ReadOnlyOverJSONThenItemArray(t *testing.T) {
	got := runReadFanOut(t, o3Object+"\n\n[\"Wychwood Clinic\", \"Marlowe Park Pharmacy\"]")
	if len(got) != 2 {
		t.Fatalf("lookups = %q, want one per customer", got)
	}
	for _, q := range got {
		if strings.ContainsAny(q, "{}[]\"") {
			t.Errorf("a child was made from a JSON fragment: %q", q)
		}
	}
}

func TestDAGExecutor_FanOut_ReadOnlyOverUnrecognisedJSONRunsOnce(t *testing.T) {
	got := runReadFanOut(t, o3Object)
	if len(got) != 1 {
		t.Fatalf("the lookup ran %d times, want once over the whole JSON: %q", len(got), got)
	}
	if !strings.Contains(got[0], "(each item listed in the result of step 0)") {
		t.Errorf("the demoted step must point at the source list; query %q", got[0])
	}
}
