package network

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Agent stack v2: a profile's `model:` preference is the first rung of the chain of
// every lease bound to that agent; other agents' chains are unchanged.
func TestLeaseChain_AgentModelPreferenceGoesFirst(t *testing.T) {
	_, gw := newLeaseServer(t)
	useFakeModel(gw, "default-gen", "x")
	gw.SetDefaultModelID("llm:default-gen")
	gw.SetAgentModelPreference(func(agentID string) string {
		if agentID == "data-analyst" {
			return "fast-gen"
		}
		return ""
	})
	chain := func(agentID string) []string {
		t.Helper()
		lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if agentID != "" {
			gw.BindLeaseAgent(lease, agentID)
		}
		lc, err := gw.beginLeaseCall(context.Background(), lease, domain.GenerateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return lc.candidates
	}
	if got := chain("data-analyst"); !slices.Equal(got, []string{"fast-gen", "default-gen"}) {
		t.Errorf("profile agent chain = %v", got)
	}
	if got := chain("assistant"); !slices.Equal(got, []string{"default-gen"}) {
		t.Errorf("agent without a preference: chain = %v", got)
	}
	if got := chain(""); !slices.Equal(got, []string{"default-gen"}) {
		t.Errorf("unbound lease: chain = %v", got)
	}
}

func TestWithPreferredModel(t *testing.T) {
	if got := withPreferredModel("llm:b", []string{"a", "b"}); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("got %v", got)
	}
	if got := withPreferredModel("", []string{"a"}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("got %v", got)
	}
}
