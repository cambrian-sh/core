// Package routescorer is the ROUTE-07 learned gatekeeper scorer (ADR-0076): a small,
// inspectable model that replaces the hand-weighted GatekeeperScore
// (w1·SuccessRate + w2·TrustScore + w3·latency − w4·cost) with a function LEARNED from
// orchestration artifacts (auction-funnel merit breakdowns joined with verifier
// outcomes). It is deliberately a logistic regression — coefficients are the feature
// weights, so the learned model is as auditable as the hand weights it competes with, and
// it is pure Go (no CGO). The online arm is default-off; adoption is gated on an OFFLINE
// win over the calibrated hand-weights baseline (offline-before-online mandate).
package routescorer

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
)

// FeatureNames are the ordered features the model scores over — the SAME quantities the
// Gatekeeper's computeMeritBreakdown produces, so training data and the online decision
// share one feature space. Keep this in lockstep with the Featurize call site.
// FeatureNames — the ORDER matches both the Gatekeeper's meritBreakdown fields (the
// online decision) and the ROUTE-02 auction-funnel MeritResultOp fields (the training
// data), so a training sample is a direct read of the funnel and no reconstruction is
// needed. latency_term/cost_term are the already-weighted contributions the funnel exposes.
var FeatureNames = []string{"success_rate", "trust_score", "latency_term", "cost_term", "provisional"}

// NumFeatures is the model's input dimension.
const NumFeatures = 5

// Sample is one training row: a candidate's decision-time merit features and its observed
// label (verifier quality in [0,1], or 1/0 for success/failure).
type Sample struct {
	Features [NumFeatures]float64 `json:"features"`
	Label    float64              `json:"label"`
}

// Model is a logistic-regression scorer: Score(x) = sigmoid(Weights·x + Bias). The
// coefficients are inspectable (each is a per-feature weight), so the learned model can be
// diffed against the hand weights it replaces.
type Model struct {
	Weights  [NumFeatures]float64 `json:"weights"`
	Bias     float64              `json:"bias"`
	Features []string             `json:"features"` // == FeatureNames, persisted for provenance
	// Mean/Std standardize each feature at fit time (and at score time), so a raw
	// latency-in-ms feature doesn't dominate a [0,1] success-rate feature.
	Mean [NumFeatures]float64 `json:"mean"`
	Std  [NumFeatures]float64 `json:"std"`
	N    int                  `json:"n"` // training-sample count (provenance / sufficiency)
}

// Score returns the model's success probability in [0,1] for a candidate's features. A nil
// model returns 0 (the caller must treat a nil scorer as "use hand weights", never as a
// zero score).
func (m *Model) Score(x [NumFeatures]float64) float64 {
	if m == nil {
		return 0
	}
	z := m.Bias
	for i := 0; i < NumFeatures; i++ {
		std := m.Std[i]
		if std == 0 {
			std = 1
		}
		z += m.Weights[i] * ((x[i] - m.Mean[i]) / std)
	}
	return sigmoid(z)
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1.0 / (1.0 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1.0 + e)
}

// FitOptions tunes the trainer.
type FitOptions struct {
	Epochs       int     // gradient-descent passes (default 500)
	LearningRate float64 // step size (default 0.1)
	L2           float64 // ridge penalty (default 1e-3) — keeps weights small on sparse data
}

// Fit trains a Model on samples by full-batch gradient descent on the log-loss with L2
// regularization. Features are standardized (mean/std stored on the model). Returns nil
// when there is no data — the caller keeps hand weights.
func Fit(samples []Sample, opts FitOptions) *Model {
	if len(samples) == 0 {
		return nil
	}
	if opts.Epochs <= 0 {
		opts.Epochs = 500
	}
	if opts.LearningRate <= 0 {
		opts.LearningRate = 0.1
	}
	if opts.L2 <= 0 {
		opts.L2 = 1e-3
	}

	m := &Model{Features: append([]string(nil), FeatureNames...), N: len(samples)}
	// Standardization stats.
	for i := 0; i < NumFeatures; i++ {
		var sum float64
		for _, s := range samples {
			sum += s.Features[i]
		}
		m.Mean[i] = sum / float64(len(samples))
	}
	for i := 0; i < NumFeatures; i++ {
		var sq float64
		for _, s := range samples {
			d := s.Features[i] - m.Mean[i]
			sq += d * d
		}
		m.Std[i] = math.Sqrt(sq / float64(len(samples)))
		if m.Std[i] == 0 {
			m.Std[i] = 1
		}
	}

	// Pre-standardize once.
	xs := make([][NumFeatures]float64, len(samples))
	for j, s := range samples {
		for i := 0; i < NumFeatures; i++ {
			xs[j][i] = (s.Features[i] - m.Mean[i]) / m.Std[i]
		}
	}

	n := float64(len(samples))
	for e := 0; e < opts.Epochs; e++ {
		var gradW [NumFeatures]float64
		var gradB float64
		for j, s := range samples {
			z := m.Bias
			for i := 0; i < NumFeatures; i++ {
				z += m.Weights[i] * xs[j][i]
			}
			err := sigmoid(z) - clamp01(s.Label)
			gradB += err
			for i := 0; i < NumFeatures; i++ {
				gradW[i] += err * xs[j][i]
			}
		}
		m.Bias -= opts.LearningRate * (gradB / n)
		for i := 0; i < NumFeatures; i++ {
			m.Weights[i] -= opts.LearningRate * (gradW[i]/n + opts.L2*m.Weights[i])
		}
	}
	return m
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

// Save writes the model as JSON.
func (m *Model) Save(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// Load reads a model from JSON. A model whose feature list does not match the current
// FeatureNames is rejected — a stale schema must not be scored silently.
func Load(r io.Reader) (*Model, error) {
	var m Model
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, err
	}
	if len(m.Features) != NumFeatures {
		return nil, fmt.Errorf("routescorer: model has %d features, expected %d", len(m.Features), NumFeatures)
	}
	for i, name := range FeatureNames {
		if m.Features[i] != name {
			return nil, fmt.Errorf("routescorer: model feature[%d]=%q, expected %q (schema drift)", i, m.Features[i], name)
		}
	}
	return &m, nil
}
