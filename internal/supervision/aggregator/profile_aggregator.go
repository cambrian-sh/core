package aggregator

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// TaskEventReader reads raw task events for aggregation.
type TaskEventReader interface {
	ReadTaskEvents(agentID, sourceHash string) ([]domain.TaskEvent, error)
	ReadAllAgentIDs() ([]string, error)
}

// AggregatorProfileStore persists computed AgentProfiles.
type AggregatorProfileStore interface {
	SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// DefaultTrustScoreCalWeight is the default calibration signal weight.
const DefaultTrustScoreCalWeight = 0.6

// DefaultTrustScoreAbsWeight is the default absolute quality signal weight.
const DefaultTrustScoreAbsWeight = 0.4

// AggregatorConfig is the subset of ExecutionConfig fields used by ProfileAggregator.
type AggregatorConfig struct {
	IntervalSeconds     int
	EWMAAlpha           float64
	LatencyWindow       int
	MinVerifiedEvents   int
	TrustScoreCalWeight float64
	TrustScoreAbsWeight float64
	HistogramMinSamples int
	HistogramAlpha      float64
	MinStepEnergy       int
	MaxStepEnergy       int
}

// tokenHistogram tracks per-step-type token utilisation distribution.
type tokenHistogram struct {
	underUsed  int // utilisation ratio [0.0, 0.5)
	normal     int // [0.5, 0.8)
	nearLimit  int // [0.8, 1.0)
	exhausted  int // [1.0, ∞)
	total      int
}

// ProfileAggregator is the background worker that periodically reads raw TaskEvent
// records and recomputes the derived Merit metrics stored as JSONB in AgentProfile.
type ProfileAggregator struct {
	Reader    TaskEventReader
	Store     AggregatorProfileStore
	Config    AggregatorConfig
	mu        sync.Mutex
	histogram map[string]*tokenHistogram // per-step-type token utilisation histogram
	Observer  domain.TelemetryObserver   // ADR-0019: may be nil (mirror only)
}

// NewProfileAggregator creates a ProfileAggregator with the given reader, store,
// and configuration.
func New(reader TaskEventReader, store AggregatorProfileStore, cfg AggregatorConfig) *ProfileAggregator {
	return &ProfileAggregator{
		Reader: reader,
		Store:  store,
		Config: cfg,
	}
}

// Start launches the background ticker loop. It blocks until ctx is cancelled.
func (a *ProfileAggregator) Start(ctx context.Context) error {
	interval := time.Duration(a.Config.IntervalSeconds) * time.Second
	slog.Info("ProfileAggregator: starting loop", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.RunOnce(ctx); err != nil {
				slog.Warn("ProfileAggregator: RunOnce error", "err", err)
			}
		}
	}
}

// RunOnce performs a single aggregation pass over all known (agentID, sourceHash) pairs.
func (a *ProfileAggregator) RunOnce(ctx context.Context) error {
	keys, err := a.Reader.ReadAllAgentIDs()
	if err != nil {
		return err
	}

	for _, key := range keys {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			slog.Warn("ProfileAggregator: malformed agentID:sourceHash key", "key", key)
			continue
		}
		agentID, sourceHash := parts[0], parts[1]

		if err := a.aggregateOne(ctx, agentID, sourceHash); err != nil {
			slog.Warn("ProfileAggregator: aggregateOne error",
				"agent_id", agentID, "source_hash", sourceHash, "err", err)
		}
	}
	return nil
}

func (a *ProfileAggregator) aggregateOne(ctx context.Context, agentID, sourceHash string) error {
	events, err := a.Reader.ReadTaskEvents(agentID, sourceHash)
	if err != nil {
		return err
	}

	var verified, unverified []domain.TaskEvent
	for _, e := range events {
		if e.Verified {
			verified = append(verified, e)
		} else {
			unverified = append(unverified, e)
		}
	}

	existing, _ := a.Store.GetProfile(ctx, agentID, sourceHash)

	var successRate float64
	if len(verified) > 0 {
		vals := make([]float64, len(verified))
		for i, e := range verified {
			if e.VerifierScore > 0.5 {
				vals[i] = 1.0
			} else {
				vals[i] = 0.0
			}
		}
		successRate = EWMA(vals, a.Config.EWMAAlpha)
	} else {
		vals := make([]float64, len(unverified))
		for i := range vals {
			vals[i] = 1.0
		}
		successRate = EWMA(vals, a.Config.EWMAAlpha)
	}

	calWeight := a.Config.TrustScoreCalWeight
	absWeight := a.Config.TrustScoreAbsWeight
	if calWeight == 0 && absWeight == 0 {
		calWeight = DefaultTrustScoreCalWeight
		absWeight = DefaultTrustScoreAbsWeight
	}

	existingProvisional := false
	if existing != nil {
		existingProvisional = existing.Provisional
	}

	minN := a.Config.MinVerifiedEvents
	stillProvisional := existingProvisional && (minN > 0 && len(verified) < minN)

	var trustScore float64
	if stillProvisional {
		trustScore = 0.5
	} else if len(verified) > 0 {
		vals := make([]float64, len(verified))
		for i, e := range verified {
			calPart := 0.0
			if e.BidConfidence > 0 {
				calPart = clamp(e.VerifierScore/e.BidConfidence, 0, 2) / 2.0
			}
			absPart := clamp(e.VerifierScore, 0, 1)
			vals[i] = calWeight*calPart + absWeight*absPart
		}
		trustScore = EWMA(vals, a.Config.EWMAAlpha)
	} else {
		trustScore = 0.5
	}

	newProvisional := existingProvisional && stillProvisional

	allEvents := events
	var netLatencies, compLatencies, contextGrowths []int
	for _, e := range allEvents {
		netLatencies = append(netLatencies, e.NetworkLatencyMs)
		compLatencies = append(compLatencies, e.ComputationLatencyMs)
		contextGrowths = append(contextGrowths, e.ContextGrowthBytes)
	}

	netMedian := RollingMedian(netLatencies, a.Config.LatencyWindow)
	compMedian := RollingMedian(compLatencies, a.Config.LatencyWindow)
	growthMedian := RollingMedian(contextGrowths, a.Config.LatencyWindow)

	var recentVerifierIDs []string
	if existing != nil {
		recentVerifierIDs = existing.RecentVerifierIDs
	}

	var modelMetrics *domain.ModelMetrics
	if len(allEvents) > 0 {
		var promptTotal, completionTotal int64
		var costTotal float64
		costVals := make([]float64, 0, len(allEvents))
		for _, e := range allEvents {
			promptTotal += int64(e.PromptTokens)
			completionTotal += int64(e.CompletionTokens)
			costTotal += e.EstimatedCost
			costVals = append(costVals, e.EstimatedCost)
		}
		avgCost := EWMA(costVals, a.Config.EWMAAlpha)
		modelMetrics = &domain.ModelMetrics{
			PromptTokensTotal:     promptTotal,
			CompletionTokensTotal: completionTotal,
			EstimatedCostTotal:    costTotal,
			AvgCostPerTask:        avgCost,
		}
	} else if existing != nil && existing.ModelMetrics != nil {
		modelMetrics = existing.ModelMetrics
	}

	profile := domain.AgentProfile{
		AgentID:                    agentID,
		SourceHash:                 sourceHash,
		SuccessRate:                successRate,
		TrustScore:                 trustScore,
		NetworkLatencyMedianMs:     int(math.Round(netMedian)),
		ComputationLatencyMedianMs: int(math.Round(compMedian)),
		ContextGrowthBytesMedian:   int(math.Round(growthMedian)),
		Provisional:                newProvisional,
		RecentVerifierIDs:          recentVerifierIDs,
		ModelMetrics:               modelMetrics,
	}

	return a.Store.SaveProfile(ctx, agentID, sourceHash, nil, profile)
}

// EWMA computes an exponentially weighted moving average over values in order.
func EWMA(values []float64, alpha float64) float64 {
	if len(values) == 0 {
		return 0.0
	}
	ewma := values[0]
	for _, v := range values[1:] {
		ewma = alpha*v + (1-alpha)*ewma
	}
	return ewma
}

// RollingMedian returns the median of the last windowSize values.
func RollingMedian(values []int, windowSize int) float64 {
	if len(values) == 0 {
		return 0.0
	}
	window := values
	if len(values) > windowSize {
		window = values[len(values)-windowSize:]
	}
	sorted := make([]int, len(window))
	copy(sorted, window)
	sort.Ints(sorted)

	n := len(sorted)
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	return float64(sorted[n/2-1]+sorted[n/2]) / 2.0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// RecordTokenUtilisation records a token utilisation observation for the given
// step type, incrementing the appropriate histogram bucket.
func (a *ProfileAggregator) RecordTokenUtilisation(stepType string, actualTokens, tokenLimit int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.histogram == nil {
		a.histogram = make(map[string]*tokenHistogram)
	}
	h, ok := a.histogram[stepType]
	if !ok {
		h = &tokenHistogram{}
		a.histogram[stepType] = h
	}
	if tokenLimit <= 0 {
		h.exhausted++
		h.total++
		return
	}
	ratio := float64(actualTokens) / float64(tokenLimit)
	switch {
	case ratio < 0.5:
		h.underUsed++
	case ratio < 0.8:
		h.normal++
	case ratio < 1.0:
		h.nearLimit++
	default:
		h.exhausted++
	}
	h.total++
}

// GetAdaptiveMaxEnergy computes an adaptively tuned max energy (token limit)
// for the given step type based on the histogram of past observations.
// Returns currentLimit unchanged if there are fewer than HistogramMinSamples
// observations, or if the histogram for stepType is nil.
func (a *ProfileAggregator) GetAdaptiveMaxEnergy(stepType string, currentLimit int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.histogram == nil {
		return currentLimit
	}
	h, ok := a.histogram[stepType]
	if !ok || h.total < a.Config.HistogramMinSamples {
		return currentLimit
	}

	underFrac := float64(h.underUsed) / float64(h.total)
	nearFrac := float64(h.nearLimit) / float64(h.total)
	exhaustedFrac := float64(h.exhausted) / float64(h.total)

	current := float64(currentLimit)
	var target float64

	if underFrac > nearFrac && underFrac > exhaustedFrac {
		target = current * 0.8
	} else if (nearFrac + exhaustedFrac) > underFrac {
		target = current * 1.25
	} else {
		target = current
	}

	alpha := a.Config.HistogramAlpha
	newLimit := current*(1-alpha) + target*alpha

	lowerBound := current * 0.8
	upperBound := current * 1.2
	if newLimit < lowerBound {
		newLimit = lowerBound
	} else if newLimit > upperBound {
		newLimit = upperBound
	}

	minVal := a.Config.MinStepEnergy
	maxVal := a.Config.MaxStepEnergy
	if minVal <= 0 {
		minVal = 256
	}
	if maxVal <= 0 {
		maxVal = 32768
	}
	newLimit = clamp(newLimit, float64(minVal), float64(maxVal))

	return int(math.Round(newLimit))
}
