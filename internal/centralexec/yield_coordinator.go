package centralexec

import (
	"fmt"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// SubGoal is the yield variant payload an agent returns instead of blocking
// (ADR-0037 D10). It is expressed in capability-space: Intent is a task
// description, NEVER an agent ID — agents are blind to the resource population
// and the Central Executive is the sole binder. ContinuationState is opaque,
// agent-owned, and returned verbatim.
type SubGoal struct {
	Intent            string
	CapabilityHint    string // optional, advisory; a soft prior, never authoritative
	Payload           *domain.Payload
	ContinuationState []byte
}

// yieldNode is one node of the open sub-goal frontier — the CE's plan graph and
// (for 0037-09) the intent-lineage tree.
type yieldNode struct {
	id              string
	parentID        string
	intentEmbedding []float32
	ancestryAgents  map[string]bool // agents bound on the path from the root to here
	boundAgent      string
	pendingCont     []byte // the yielding agent's resume state while its child runs
}

// YieldCoordinator owns the open sub-goal frontier (ADR-0037 D10): it splices
// yielded sub-goals into the live plan, stores opaque continuation_state, drives
// the standard capability-grounded binding for each sub-goal, and re-dispatches
// the parent on completion. It owns O(1) ancestry cycle detection and the D15
// liveness (semantic-narrowing) guard. There is exactly one coordinator per
// frontier — yield, not recursion (D10).
type YieldCoordinator struct {
	mu sync.Mutex
	// NarrowingMargin is the D15 guard: a child intent that is not at least this
	// much less similar to its parent (cosine) is the livelock signature and is
	// rejected. A sub-goal must be a strict refinement of its parent.
	NarrowingMargin float64
	nodes           map[string]*yieldNode
	counter         int
}

// NewYieldCoordinator constructs an empty coordinator.
func NewYieldCoordinator(narrowingMargin float64) *YieldCoordinator {
	return &YieldCoordinator{
		NarrowingMargin: narrowingMargin,
		nodes:           map[string]*yieldNode{},
	}
}

func (c *YieldCoordinator) nextID() string {
	c.counter++
	return fmt.Sprintf("node-%d", c.counter)
}

// OpenRoot starts a frontier rooted at a top-level intent and returns its id.
func (c *YieldCoordinator) OpenRoot(intentEmbedding []float32) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID()
	c.nodes[id] = &yieldNode{
		id:              id,
		intentEmbedding: intentEmbedding,
		ancestryAgents:  map[string]bool{},
	}
	return id
}

// ErrLivelock is returned when a sub-goal fails the D15 narrowing guard.
var ErrLivelock = fmt.Errorf("yield: sub-goal not a strict refinement of its parent (livelock guard)")

// Yield splices a sub-goal under a parent node, storing the parent's
// continuation_state for stateless resume. It enforces the D15 narrowing guard
// against the parent's intent embedding. Returns the new child node id.
func (c *YieldCoordinator) Yield(parentID string, sg SubGoal, childEmbedding []float32) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	parent, ok := c.nodes[parentID]
	if !ok {
		return "", fmt.Errorf("yield: unknown parent %q", parentID)
	}

	// D15 liveness: the child must be a strict refinement — sufficiently distinct
	// from its parent. A near-identical intent is the livelock signature.
	sim := domain.CosineSimilarity(parent.intentEmbedding, childEmbedding)
	if sim > 1.0-c.NarrowingMargin {
		return "", ErrLivelock
	}

	// Inherit the parent's bound-agent ancestry (the visited set for cycle
	// checks) plus the parent's own bound agent.
	ancestry := make(map[string]bool, len(parent.ancestryAgents)+1)
	for a := range parent.ancestryAgents {
		ancestry[a] = true
	}
	if parent.boundAgent != "" {
		ancestry[parent.boundAgent] = true
	}

	id := c.nextID()
	c.nodes[id] = &yieldNode{
		id:              id,
		parentID:        parentID,
		intentEmbedding: childEmbedding,
		ancestryAgents:  ancestry,
	}
	parent.pendingCont = sg.ContinuationState
	return id, nil
}

// ErrCycle is returned when binding an agent already in a sub-goal's ancestry.
var ErrCycle = fmt.Errorf("yield: agent already in sub-goal ancestry (cycle)")

// BindResource records the resource bound to a sub-goal node. It rejects a
// candidate already present in the node's ancestry — an O(1) set lookup on the
// node's own visited set, no distributed stack trace (ADR-0037 D15 #2).
func (c *YieldCoordinator) BindResource(nodeID, agentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.nodes[nodeID]
	if !ok {
		return fmt.Errorf("bind: unknown node %q", nodeID)
	}
	if n.ancestryAgents[agentID] {
		return ErrCycle
	}
	n.boundAgent = agentID
	return nil
}

// Resume returns the continuation_state stored for a parent whose child has
// completed, verbatim. ok is false if there is no pending continuation.
func (c *YieldCoordinator) Resume(parentID string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.nodes[parentID]
	if !ok || n.pendingCont == nil {
		return nil, false
	}
	return n.pendingCont, true
}
