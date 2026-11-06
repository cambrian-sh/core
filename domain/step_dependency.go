package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

// StepDependency is one upstream result delivered to a dependent plan step: the
// output of a step named in that step's DependsOn.
//
// The DAG executor builds the list once per dispatched step (executer.buildStepDependencies)
// and delivers it the same way whichever executor runs the step: encoded under
// HandoffKeyStepDependencies in the handoff's metadata. The in-kernel thought step
// renders it into its synthesis request; an SDK agent renders it as an evidence region
// (cambrian_agent_sdk/step_dependencies.py). It is DATA produced by other agents and
// tools — receivers must mark it as such and never follow instructions inside it.
//
// Before this type existed the results travelled only as anonymous working-memory
// refs or step_N_result context keys, and the thought step's request ignored both:
// a merge step answered "I don't see any previous steps"
// (T-20261105-a-planner-thought-step-that-depends-on-two).
type StepDependency struct {
	// Step is the upstream step's index in the plan.
	Step int `json:"step"`
	// Objective is the upstream step's query: what it was asked to produce.
	Objective string `json:"objective"`
	// Status is one of the StepDependency* constants. An empty or unavailable upstream
	// step is listed with that status, never left out.
	Status string `json:"status"`
	// CID addresses the full result in the content store; "" when no store is wired
	// or the result is empty.
	CID string `json:"cid,omitempty"`
	// Bytes is the size of the full result.
	Bytes int `json:"bytes"`
	// Content is the result, whole unless Shortened.
	Content string `json:"content,omitempty"`
	// Shortened marks Content as a preview (its first len(Content) bytes) of a result
	// larger than its share of the budget; the rest is resolvable by CID.
	Shortened bool `json:"shortened,omitempty"`
	// PriorPlan is 0 for a step of the running plan. N >= 1 marks a result carried
	// forward from the N-th plan a replan swapped out of this run (InjectCorrection,
	// hot-swap, error replan): Step then indexes THAT plan, not the current one, and
	// receivers label it prior_step_<Step>. Such entries reach the replacement plan's
	// steps that declare no in-plan dependency of their own
	// (T-20261106-an-injected-injectcorrection-step-cannot-see).
	PriorPlan int `json:"prior_plan,omitempty"`
	// Error is why a StepDependencyFailed step failed.
	Error string `json:"error,omitempty"`
}

// StepDependency statuses.
const (
	// StepDependencyCompleted: the step finished and produced output.
	StepDependencyCompleted = "completed"
	// StepDependencyEmpty: the step finished and produced no output.
	StepDependencyEmpty = "empty"
	// StepDependencyUnavailable: the step counts as done (a resumed run's earlier
	// steps) but no result for it was recorded.
	StepDependencyUnavailable = "unavailable"
	// StepDependencyFailed: the step failed (Error says why). Only a prior-plan entry
	// can carry it: within one plan a failed step never dispatches its dependents.
	StepDependencyFailed = "failed"
)

// HandoffKeyStepDependencies is the handoff metadata key carrying a dependent step's
// JSON-encoded []StepDependency. Kernel-authored: the executor overwrites it on every
// dispatch and strips it from responses, so an agent can neither forge nor echo it
// into the plan context.
const HandoffKeyStepDependencies = "_step_dependencies"

// EncodeStepDependencies renders deps for HandoffKeyStepDependencies.
func EncodeStepDependencies(deps []StepDependency) (string, error) {
	b, err := json.Marshal(deps)
	if err != nil {
		return "", fmt.Errorf("encode step dependencies: %w", err)
	}
	return string(b), nil
}

// DecodeStepDependencies reads HandoffKeyStepDependencies. "" decodes to nil.
func DecodeStepDependencies(s string) ([]StepDependency, error) {
	if s == "" {
		return nil, nil
	}
	var deps []StepDependency
	if err := json.Unmarshal([]byte(s), &deps); err != nil {
		return nil, fmt.Errorf("decode step dependencies: %w", err)
	}
	return deps, nil
}

// FitStepDependencies bounds the inlined Content of deps to budget bytes in total.
// Every dependency keeps a fair share: the budget is split evenly, a result smaller
// than its share gives the rest back to the others (water-filling), and only results
// larger than the final share are cut — on a rune boundary — and marked Shortened, so
// nothing is truncated silently. deps is modified in place and returned. budget <= 0
// leaves deps whole.
func FitStepDependencies(deps []StepDependency, budget int) []StepDependency {
	if budget <= 0 || len(deps) == 0 {
		return deps
	}
	total := 0
	sizes := make([]int, len(deps))
	for i, d := range deps {
		sizes[i] = len(d.Content)
		total += sizes[i]
	}
	if total <= budget {
		return deps
	}
	sort.Ints(sizes)
	share, left := 0, budget
	for i, n := range sizes {
		even := left / (len(sizes) - i)
		if n > even {
			share = even
			break
		}
		left -= n
		share = n
	}
	for i := range deps {
		c := deps[i].Content
		if len(c) <= share {
			continue
		}
		cut := share
		for cut > 0 && !utf8.RuneStart(c[cut]) {
			cut--
		}
		deps[i].Content = c[:cut]
		deps[i].Shortened = true
	}
	return deps
}
