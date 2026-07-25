package domain_test

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func TestSessionToken_ZeroValue(t *testing.T) {
	var st domain.BudgetLease
	if st.ID != "" {
		t.Errorf("BudgetLease.ID zero value = %q, want empty string", st.ID)
	}
}

func TestSessionToken_New(t *testing.T) {
	st := domain.BudgetLease{ID: "abc-123"}
	if st.ID != "abc-123" {
		t.Errorf("BudgetLease.ID = %q, want %q", st.ID, "abc-123")
	}
}

func TestSessionState_New(t *testing.T) {
	now := time.Now()
	ss := domain.BudgetLeaseState{
		StepAllocation:   domain.StepAllocation{},
		ConsumedTokens:   0,
		ActualTokensUsed: 75,
		ExpiresAt:        now,
		LastActivityAt:   now,
	}
	if ss.ActualTokensUsed != 75 {
		t.Errorf("BudgetLeaseState.ActualTokensUsed = %d, want 75", ss.ActualTokensUsed)
	}
	if ss.ExpiresAt != now {
		t.Errorf("BudgetLeaseState.ExpiresAt = %v, want %v", ss.ExpiresAt, now)
	}
}

func TestStepAllocation_New(t *testing.T) {
	winner := domain.AgentDefinition{ID: "winner-1"}
	fb0 := domain.AgentDefinition{ID: "fallback-1"}
	fb1 := domain.AgentDefinition{ID: "fallback-2"}

	sa := domain.StepAllocation{
		Winner:    winner,
		Fallbacks: [2]domain.AgentDefinition{fb0, fb1},
	}

	if sa.Winner.ID != "winner-1" {
		t.Errorf("StepAllocation.Winner.ID = %q, want %q", sa.Winner.ID, "winner-1")
	}
	if sa.Fallbacks[0].ID != "fallback-1" {
		t.Errorf("StepAllocation.Fallbacks[0].ID = %q, want %q", sa.Fallbacks[0].ID, "fallback-1")
	}
	if sa.Fallbacks[1].ID != "fallback-2" {
		t.Errorf("StepAllocation.Fallbacks[1].ID = %q, want %q", sa.Fallbacks[1].ID, "fallback-2")
	}
}

func TestGenerateOptions_New(t *testing.T) {
	opts := domain.GenerateOptions{
		MaxTokens:     512,
		Temperature:   0.7,
		StopSequences: []string{"END", "STOP"},
	}

	if opts.MaxTokens != 512 {
		t.Errorf("GenerateOptions.MaxTokens = %d, want 512", opts.MaxTokens)
	}
	if opts.Temperature != 0.7 {
		t.Errorf("GenerateOptions.Temperature = %f, want 0.7", opts.Temperature)
	}
	if opts.StopSequences[0] != "END" {
		t.Errorf("GenerateOptions.StopSequences[0] = %q, want %q", opts.StopSequences[0], "END")
	}
}

func TestHandoff_SessionToken_NilSafe(t *testing.T) {
	h := domain.Handoff{}
	if h.BudgetLease != nil {
		t.Errorf("Handoff.BudgetLease zero value = %v, want nil", h.BudgetLease)
	}
	st := domain.BudgetLease{ID: "tok-001"}
	h.BudgetLease = &st
	if h.BudgetLease.ID != "tok-001" {
		t.Errorf("Handoff.BudgetLease.ID = %q, want %q", h.BudgetLease.ID, "tok-001")
	}
}

func TestAgentManifest_RequiredModelCapabilities(t *testing.T) {
	m := domain.AgentManifest{}
	if len(m.RequiredModelCapabilities) != 0 {
		t.Errorf("AgentManifest.RequiredModelCapabilities zero value = %v, want empty", m.RequiredModelCapabilities)
	}
	m.RequiredModelCapabilities = []string{"code_generation", "data_analysis"}
	if len(m.RequiredModelCapabilities) != 2 {
		t.Errorf("len(RequiredModelCapabilities) = %d, want 2", len(m.RequiredModelCapabilities))
	}
}

func TestTaskEvent_GatewayFields(t *testing.T) {
	evt := domain.TaskEvent{}
	if evt.BudgetOverrun {
		t.Errorf("TaskEvent.BudgetOverrun zero value = true, want false")
	}
	if evt.FallbackModelUsed {
		t.Errorf("TaskEvent.FallbackModelUsed zero value = true, want false")
	}
	if evt.ActualModelID != "" {
		t.Errorf("TaskEvent.ActualModelID zero value = %q, want empty", evt.ActualModelID)
	}
	evt.BudgetOverrun = true
	evt.FallbackModelUsed = true
	evt.ActualModelID = "model-001"
	if !evt.BudgetOverrun {
		t.Errorf("TaskEvent.BudgetOverrun = false, want true")
	}
	if !evt.FallbackModelUsed {
		t.Errorf("TaskEvent.FallbackModelUsed = false, want true")
	}
	if evt.ActualModelID != "model-001" {
		t.Errorf("TaskEvent.ActualModelID = %q, want %q", evt.ActualModelID, "model-001")
	}
}

func TestAuctionResult_StepAllocation_NilSafe(t *testing.T) {
	ar := domain.AuctionResult{}
	if ar.StepAllocation != nil {
		t.Errorf("AuctionResult.StepAllocation zero value = %v, want nil", ar.StepAllocation)
	}
	sa := domain.StepAllocation{
		Winner: domain.AgentDefinition{ID: "winner"},
	}
	ar.StepAllocation = &sa
	if ar.StepAllocation.Winner.ID != "winner" {
		t.Errorf("StepAllocation.Winner.ID = %q, want %q", ar.StepAllocation.Winner.ID, "winner")
	}
}
