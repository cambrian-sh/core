package centralexec

import "github.com/cambrian-sh/core/domain"

// Credit is a precision signal attributed to one resource on one channel.
type Credit struct {
	ResourceID string
	Amount     float64
}

// CreditResult is the two-channel attribution of a sub-goal outcome (ADR-0037
// D12). Execution credit goes to the resource that did the work; Decomposition
// credit goes to the parent that framed the sub-goal, scaled by information
// added. A parent never receives execution credit for a child's work.
type CreditResult struct {
	Execution     Credit
	Decomposition Credit
}

// AttributeOutcome walks the intent-lineage graph (which is the CE's own plan
// graph, D13) and splits a sub-goal's outcome quality across the two precision
// channels (D12). The decomposition channel is counterfactual: it credits the
// parent only for information added — how much the sub-goal intent narrowed the
// task beyond the parent's. A pass-through (near-identical intent) adds nothing
// and earns ~0, blocking the laundering vector.
func (c *YieldCoordinator) AttributeOutcome(nodeID string, quality float64) CreditResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, ok := c.nodes[nodeID]
	if !ok {
		return CreditResult{}
	}

	res := CreditResult{
		Execution: Credit{ResourceID: node.boundAgent, Amount: quality},
	}

	if node.parentID != "" {
		if parent, ok := c.nodes[node.parentID]; ok {
			info := informationAdded(parent.intentEmbedding, node.intentEmbedding)
			res.Decomposition = Credit{
				ResourceID: parent.boundAgent,
				Amount:     quality * info,
			}
		}
	}
	return res
}

// informationAdded is the counterfactual decomposition measure: how much a
// sub-goal intent narrowed/clarified the task beyond its parent, proxied as
// 1 - cosine(parent, child) clamped to [0,1] (ADR-0037 D12). A pass-through
// (cosine ≈ 1) adds ≈ 0.
func informationAdded(parentEmb, childEmb []float32) float64 {
	v := 1.0 - domain.CosineSimilarity(parentEmb, childEmb)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
