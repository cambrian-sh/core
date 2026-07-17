package routescorer

import "sort"

// HandWeights are the baseline GatekeeperScore coefficients (the ROUTE-05/06 calibrated
// arm) the learned model must beat to be adopted. The feature vector already carries the
// WEIGHTED latency_term/cost_term (as the funnel exposes them), so the baseline is exactly
// the Gatekeeper's computeMeritBreakdown: W1·success_rate + W2·trust_score + latency_term
// − cost_term. (The provisional penalty is a post-multiplier applied equally, so it is
// excluded — it does not change the ranking.)
type HandWeights struct{ W1, W2 float64 }

// Score returns the hand-weighted baseline score for a feature vector.
func (h HandWeights) Score(x [NumFeatures]float64) float64 {
	return h.W1*x[0] + h.W2*x[1] + x[2] - x[3]
}

// AUC is the area under the ROC curve for a scorer over labeled samples: the probability
// that a randomly chosen positive (label ≥ 0.5) outranks a randomly chosen negative. 0.5
// is chance; 1.0 is perfect. Computed by the rank-sum (Mann–Whitney) identity, so it is
// threshold-free — the right offline metric for "does this scorer RANK the agent that
// will succeed above the one that won't". Returns -1 when a class is empty.
func AUC(scores []float64, labels []float64) float64 {
	type sl struct {
		score float64
		pos   bool
	}
	rows := make([]sl, len(scores))
	var nPos, nNeg int
	for i := range scores {
		pos := labels[i] >= 0.5
		rows[i] = sl{scores[i], pos}
		if pos {
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return -1
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score < rows[j].score })
	// Average ranks (1-based), handling ties by mean rank.
	var rankSumPos float64
	i := 0
	for i < len(rows) {
		j := i
		for j < len(rows) && rows[j].score == rows[i].score {
			j++
		}
		avgRank := float64(i+1+j) / 2.0 // mean of ranks [i+1 .. j]
		for k := i; k < j; k++ {
			if rows[k].pos {
				rankSumPos += avgRank
			}
		}
		i = j
	}
	return (rankSumPos - float64(nPos)*float64(nPos+1)/2.0) / (float64(nPos) * float64(nNeg))
}

// Comparison is the offline result: the learned model's AUC vs the hand-weight baseline's
// AUC on the SAME held-out samples, plus the decision the gate implies.
type Comparison struct {
	N            int     `json:"n"`
	LearnedAUC   float64 `json:"learned_auc"`
	HandAUC      float64 `json:"hand_auc"`
	Delta        float64 `json:"delta"`         // learned − hand
	NoiseMargin  float64 `json:"noise_margin"`  // a delta within ±margin is "within noise"
	AdoptLearned bool    `json:"adopt_learned"` // learned beats hand by more than the margin
}

// CompareOnHeldout scores both arms over held-out samples and returns the gate comparison.
// margin is the minimum AUC delta to call a win (default 0.01) — "within-noise → keep hand
// weights (simpler, inspectable)" per the ROUTE-07 gate.
func CompareOnHeldout(m *Model, hw HandWeights, heldout []Sample, margin float64) Comparison {
	if margin <= 0 {
		margin = 0.01
	}
	learned := make([]float64, len(heldout))
	hand := make([]float64, len(heldout))
	labels := make([]float64, len(heldout))
	for i, s := range heldout {
		learned[i] = m.Score(s.Features)
		hand[i] = hw.Score(s.Features)
		labels[i] = s.Label
	}
	la, ha := AUC(learned, labels), AUC(hand, labels)
	delta := la - ha
	return Comparison{
		N:            len(heldout),
		LearnedAUC:   la,
		HandAUC:      ha,
		Delta:        delta,
		NoiseMargin:  margin,
		AdoptLearned: delta > margin,
	}
}

// Split partitions samples into train/test by a deterministic stride (every kth sample to
// test), so the offline eval is reproducible. testFrac in (0,1); default 0.2.
func Split(samples []Sample, testFrac float64) (train, test []Sample) {
	if testFrac <= 0 || testFrac >= 1 {
		testFrac = 0.2
	}
	stride := int(1.0 / testFrac)
	if stride < 2 {
		stride = 2
	}
	for i, s := range samples {
		if i%stride == 0 {
			test = append(test, s)
		} else {
			train = append(train, s)
		}
	}
	return train, test
}
