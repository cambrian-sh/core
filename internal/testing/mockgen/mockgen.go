package mockgen

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Scenario defines the distributions for synthetic TaskEvent generation.
type Scenario struct {
	PromptTokensMean         float64
	PromptTokensStddev       float64
	CompletionTokensMean     float64
	CompletionTokensStddev   float64
	TokenLimitPerStep        int
	FallbackRate             float64
	SchemaMismatchRate       float64
}

var Baseline = Scenario{
	PromptTokensMean:     500, PromptTokensStddev: 100,
	CompletionTokensMean: 800, CompletionTokensStddev: 150,
	TokenLimitPerStep: 1024, FallbackRate: 0.02, SchemaMismatchRate: 0.01,
}

var Adversarial = Scenario{
	PromptTokensMean:     600, PromptTokensStddev: 100,
	CompletionTokensMean: 1200, CompletionTokensStddev: 200,
	TokenLimitPerStep: 512, FallbackRate: 0.05, SchemaMismatchRate: 0.02,
}

var Noisy = Scenario{
	PromptTokensMean:     500, PromptTokensStddev: 400,
	CompletionTokensMean:  800, CompletionTokensStddev: 600,
	TokenLimitPerStep: 1024, FallbackRate: 0.03, SchemaMismatchRate: 0.02,
}

var ColdStart = Scenario{
	PromptTokensMean:     300, PromptTokensStddev: 80,
	CompletionTokensMean:  400, CompletionTokensStddev: 100,
	TokenLimitPerStep: 768, FallbackRate: 0.15, SchemaMismatchRate: 0.05,
}

var BudgetCrisis = Scenario{
	PromptTokensMean:     400, PromptTokensStddev: 80,
	CompletionTokensMean:  400, CompletionTokensStddev: 60,
	TokenLimitPerStep: 300, FallbackRate: 0.02, SchemaMismatchRate: 0.01,
}

func normalSample(rng *rand.Rand, mean, stddev float64) float64 {
	return rng.NormFloat64()*stddev + mean
}

func clampToNonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// GenerateTelemetryCorpus produces n synthetic TaskEvent records.
// BudgetOverrun is derived from completionTokens > effectiveTokenLimit.
func GenerateTelemetryCorpus(n int, scenario Scenario, cfg *config.ExecutionConfig, seed int64) []domain.TaskEvent {
	rng := rand.New(rand.NewSource(seed))
	events := make([]domain.TaskEvent, 0, n)
	baseTime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	limit := scenario.TokenLimitPerStep
	if cfg != nil && cfg.MinStepEnergy > 0 {
		limit = cfg.MinStepEnergy
	}

	for i := 0; i < n; i++ {
		promptTokens := int(math.Round(clampToNonNeg(normalSample(rng,
			scenario.PromptTokensMean, scenario.PromptTokensStddev))))
		completionTokens := int(math.Round(clampToNonNeg(normalSample(rng,
			scenario.CompletionTokensMean, scenario.CompletionTokensStddev))))

		effLimit := limit
		if effLimit <= 0 {
			effLimit = 1024
		}

		events = append(events, domain.TaskEvent{
			TaskID:               fmt.Sprintf("task-%08d", i),
			AgentID:              "agent-test",
			SourceHash:           "hash-001",
			BidConfidence:        clamp01(rng.Float64()*0.3 + 0.7),
			VerifierScore:        clamp01(rng.Float64()),
			NetworkLatencyMs:     int(math.Round(clampToNonNeg(rng.NormFloat64()*10 + 50))),
			ComputationLatencyMs: int(math.Round(clampToNonNeg(rng.NormFloat64()*50 + 200))),
			PromptTokens:         promptTokens,
			CompletionTokens:     completionTokens,
			TotalTokens:          promptTokens + completionTokens,
			EstimatedCost:        float64(promptTokens+completionTokens) * 0.000001,
			BudgetOverrun:        completionTokens > effLimit,
			FallbackModelUsed:    rng.Float64() < scenario.FallbackRate,
			ActualModelID:        "model-001",
			Timestamp:             baseTime.Add(time.Duration(i) * time.Second),
		})
	}
	return events
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
