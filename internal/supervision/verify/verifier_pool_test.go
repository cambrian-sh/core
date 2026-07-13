package verify

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestVerifierPool_Select_ReturnsBestVerifier(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "v1", SourceHash: "s1", Provisional: false},
		{ID: "v2", SourceHash: "s2", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"v1:s1": {TrustScore: 0.85, SuccessRate: 0.9},
		"v2:s2": {TrustScore: 0.95, SuccessRate: 0.92},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t1"}

	got, err := pool.Select(context.Background(), task, "other-agent", nil)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.ID != "v2" {
		t.Errorf("expected verifier v2 (highest TrustScore), got %q", got.ID)
	}
}

func TestVerifierPool_Select_ExcludesWinner(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "winner", SourceHash: "sw", Provisional: false},
		{ID: "verifier", SourceHash: "sv", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"winner:sw":   {TrustScore: 0.99, SuccessRate: 0.99},
		"verifier:sv": {TrustScore: 0.85, SuccessRate: 0.85},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t2"}

	got, err := pool.Select(context.Background(), task, "winner", nil)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.ID == "winner" {
		t.Error("Select must not return the original winner as verifier")
	}
}

func TestVerifierPool_Select_ExcludesRecentVerifiers(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "recent", SourceHash: "sr", Provisional: false},
		{ID: "fresh", SourceHash: "sf", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"recent:sr": {TrustScore: 0.95, SuccessRate: 0.95},
		"fresh:sf":  {TrustScore: 0.82, SuccessRate: 0.82},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t3"}
	subjectProfile := &domain.AgentProfile{
		RecentVerifierIDs: []string{"recent"},
	}

	got, err := pool.Select(context.Background(), task, "other", subjectProfile)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.ID == "recent" {
		t.Error("Select must not return a recently used verifier")
	}
	if got.ID != "fresh" {
		t.Errorf("expected verifier 'fresh', got %q", got.ID)
	}
}

func TestVerifierPool_Select_NoEligible_ReturnsError(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "only", SourceHash: "so", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"only:so": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t4"}

	_, err := pool.Select(context.Background(), task, "only", nil)
	if err != ErrNoVerifierAvailable {
		t.Errorf("expected ErrNoVerifierAvailable, got %v", err)
	}
}

func TestVerifierPool_Select_BelowThreshold_Excluded(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "low", SourceHash: "sl", Provisional: false},
		{ID: "high", SourceHash: "sh", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"low:sl":  {TrustScore: 0.5, SuccessRate: 0.9},
		"high:sh": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t5"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.ID == "low" {
		t.Error("Select must not return a verifier below the pool threshold")
	}
}

func TestVerifierPool_Select_ExcludesProvisional(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "prov", SourceHash: "sp", Provisional: true},
		{ID: "active", SourceHash: "sa", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"prov:sp":   {TrustScore: 0.95, SuccessRate: 0.95},
		"active:sa": {TrustScore: 0.85, SuccessRate: 0.85},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t6"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.ID == "prov" {
		t.Error("Select must not return a Provisional agent as verifier")
	}
}

func TestVerifierPool_HealthGuard_RelaxesThreshold(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "high", SourceHash: "sh", Provisional: false},
		{ID: "medium", SourceHash: "sm", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"high:sh":   {TrustScore: 0.85, SuccessRate: 0.85},
		"medium:sm": {TrustScore: 0.72, SuccessRate: 0.72},
	}
	pool := &VerifierPool{
		Registry:       newAgentSourceWith(agents...),
		Profiles:       &vwMockGatekeeperReader{profiles: profiles},
		Threshold:      0.80,
		RecencyWindow:  3,
		MinSize:        2,
		ThresholdStep:  0.05,
		ThresholdFloor: 0.60,
	}
	task := &domain.AuctionTask{ID: "t-guard"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err != nil {
		t.Fatalf("Select error after guard relaxation: %v", err)
	}
	if got.ID != "high" {
		t.Errorf("expected 'high' (best TrustScore), got %q", got.ID)
	}
}

func TestVerifierPool_HealthGuard_ReturnsBestAtFloor(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "lone", SourceHash: "sl", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"lone:sl": {TrustScore: 0.65, SuccessRate: 0.65},
	}
	pool := &VerifierPool{
		Registry:       newAgentSourceWith(agents...),
		Profiles:       &vwMockGatekeeperReader{profiles: profiles},
		Threshold:      0.80,
		RecencyWindow:  3,
		MinSize:        2,
		ThresholdStep:  0.05,
		ThresholdFloor: 0.60,
	}
	task := &domain.AuctionTask{ID: "t-floor"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err != nil {
		t.Fatalf("expected 1 qualifying agent at floor, got error: %v", err)
	}
	if got.ID != "lone" {
		t.Errorf("expected 'lone', got %q", got.ID)
	}
}

func TestVerifierPool_HealthGuard_TotalCollapse_ReturnsError(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "bad", SourceHash: "sb", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"bad:sb": {TrustScore: 0.55, SuccessRate: 0.55},
	}
	pool := &VerifierPool{
		Registry:       newAgentSourceWith(agents...),
		Profiles:       &vwMockGatekeeperReader{profiles: profiles},
		Threshold:      0.80,
		RecencyWindow:  3,
		MinSize:        2,
		ThresholdStep:  0.05,
		ThresholdFloor: 0.60,
	}
	task := &domain.AuctionTask{ID: "t-collapse"}

	_, err := pool.Select(context.Background(), task, "other", nil)
	if err != ErrNoVerifierAvailable {
		t.Errorf("expected ErrNoVerifierAvailable on total collapse, got %v", err)
	}
}

func TestVerifierPool_Select_NeverReturnsTraitTool(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "cognitive", SourceHash: "sc", Provisional: false, Trait: domain.TraitCognitive},
		{ID: "tool", SourceHash: "st", Provisional: false, Trait: domain.TraitTool},
	}
	profiles := map[string]*domain.AgentProfile{
		"cognitive:sc": {TrustScore: 0.85, SuccessRate: 0.85},
		"tool:st":      {TrustScore: 0.95, SuccessRate: 0.95},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t-trait-exclusion"}

	for i := 0; i < 10; i++ {
		got, err := pool.Select(context.Background(), task, "other", nil)
		if err != nil {
			t.Fatalf("Select error on call %d: %v", i+1, err)
		}
		if got.ID == "tool" {
			t.Errorf("call %d: Select returned TraitTool agent %q; TraitTool must never be a verifier", i+1, got.ID)
		}
	}
}

func TestVerifierPool_Select_NeverReturnsTraitDaemon(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "cognitive", SourceHash: "sc", Provisional: false, Trait: domain.TraitCognitive},
		{ID: "daemon", SourceHash: "sd", Provisional: false, Trait: domain.TraitDaemon},
	}
	profiles := map[string]*domain.AgentProfile{
		"cognitive:sc": {TrustScore: 0.85, SuccessRate: 0.85},
		"daemon:sd":    {TrustScore: 0.95, SuccessRate: 0.95},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t-daemon-exclusion"}

	for i := 0; i < 10; i++ {
		got, err := pool.Select(context.Background(), task, "other", nil)
		if err != nil {
			t.Fatalf("Select error on call %d: %v", i+1, err)
		}
		if got.ID == "daemon" {
			t.Errorf("call %d: Select returned TraitDaemon agent %q; TraitDaemon must never be a verifier", i+1, got.ID)
		}
	}
}

func TestVerifierPool_Select_OnlyTraitDaemon_ReturnsError(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "daemon1", SourceHash: "sd1", Provisional: false, Trait: domain.TraitDaemon},
		{ID: "daemon2", SourceHash: "sd2", Provisional: false, Trait: domain.TraitDaemon},
	}
	profiles := map[string]*domain.AgentProfile{
		"daemon1:sd1": {TrustScore: 0.9, SuccessRate: 0.9},
		"daemon2:sd2": {TrustScore: 0.95, SuccessRate: 0.95},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t-daemon-only"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err == nil {
		t.Errorf("Select returned agent %q; expected ErrNoVerifierAvailable when only TraitDaemon agents exist", got.ID)
	}
	if got != nil && got.Trait == domain.TraitDaemon {
		t.Error("Select must not return a TraitDaemon agent regardless of threshold")
	}
}

func TestVerifierPool_Select_OnlyTraitTool_ReturnsError(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "tool1", SourceHash: "st1", Provisional: false, Trait: domain.TraitTool},
		{ID: "tool2", SourceHash: "st2", Provisional: false, Trait: domain.TraitTool},
	}
	profiles := map[string]*domain.AgentProfile{
		"tool1:st1": {TrustScore: 0.9, SuccessRate: 0.9},
		"tool2:st2": {TrustScore: 0.95, SuccessRate: 0.95},
	}
	pool := newTestVerifierPool(agents, profiles, 0.8)
	task := &domain.AuctionTask{ID: "t-tool-only"}

	got, err := pool.Select(context.Background(), task, "other", nil)
	if err == nil {
		t.Errorf("Select returned agent %q; expected ErrNoVerifierAvailable when only TraitTool agents exist", got.ID)
	}
	if got != nil && got.Trait == domain.TraitTool {
		t.Error("Select must not return a TraitTool agent regardless of threshold")
	}
}
