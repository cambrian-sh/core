package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FanOutWidthError is returned when a discovered set exceeds the configured expansion
// width. Deliberately a hard, structured error rather than a silent truncation
// (ADR-0051 D10): the caller routes it to the ReplanHandler so the planner reshapes the
// work, instead of quietly dropping items the user asked for.
type FanOutWidthError struct {
	StepIndex int
	Width     int
	MaxWidth  int
}

func (e *FanOutWidthError) Error() string {
	return fmt.Sprintf("fan-out at step %d would expand to %d children, exceeding the max width %d",
		e.StepIndex, e.Width, e.MaxWidth)
}

// fanOutItemKeys are the object fields, in preference order, that carry an item's
// identity when a source step emits an array of objects rather than of strings.
var fanOutItemKeys = []string{"id", "item", "name", "path", "title"}

// fanOutArrayKeys are the object fields, in preference order, that carry the set when a
// source step emits a wrapper object rather than a bare array.
var fanOutArrayKeys = []string{"items", "entities", "missing", "results", "files", "list"}

// ExtractFanOutItems deterministically pulls the item set out of a source step's output
// (ADR-0051 D10: "extracted deterministically, no LLM re-parse"). It accepts, in order:
// a JSON array (of strings, numbers, or objects keyed by id/item/name/path/title); a JSON
// object wrapping such an array under a known key (or its single array-valued field); and
// otherwise falls back to non-empty trimmed lines. Duplicates are dropped, order is
// preserved, so expansion is stable for the same output.
func ExtractFanOutItems(raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if items, ok := parseJSONItems(s); ok {
		return dedupe(items)
	}
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	return dedupe(lines)
}

// parseJSONItems handles the JSON shapes; ok=false means "not JSON we understand".
func parseJSONItems(s string) ([]string, bool) {
	switch s[0] {
	case '[':
		var arr []any
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return nil, false
		}
		return itemsFromArray(arr), true
	case '{':
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, false
		}
		for _, k := range fanOutArrayKeys {
			if arr, ok := obj[k].([]any); ok {
				return itemsFromArray(arr), true
			}
		}
		// Exactly one array-valued field ⇒ unambiguous; more than one ⇒ refuse to guess.
		var found []any
		n := 0
		for _, v := range obj {
			if arr, ok := v.([]any); ok {
				found, n = arr, n+1
			}
		}
		if n == 1 {
			return itemsFromArray(found), true
		}
		return nil, false
	}
	return nil, false
}

func itemsFromArray(arr []any) []string {
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		switch v := e.(type) {
		case string:
			if t := strings.TrimSpace(v); t != "" {
				out = append(out, t)
			}
		case float64:
			out = append(out, strings.TrimSuffix(fmt.Sprintf("%v", v), ".0"))
		case map[string]any:
			for _, k := range fanOutItemKeys {
				if sv, ok := v[k].(string); ok {
					if t := strings.TrimSpace(sv); t != "" {
						out = append(out, t)
						break
					}
				}
			}
		}
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// FanOutVarName returns the template variable for a step ("" ⇒ the "item" default).
func (s Step) FanOutVarName() string {
	if strings.TrimSpace(s.FanOutVar) == "" {
		return "item"
	}
	return strings.TrimSpace(s.FanOutVar)
}

// ExpandFanOut rewrites plan by APPENDING one concrete child per item (template
// substitution of the parametric step's fan-out variable) at fresh indices, leaving every
// existing step index untouched. Dependents of the parametric node are rewritten to depend
// on all children instead — a BARRIER / reduce (ADR-0051 D10). The parametric node itself
// stays in place but INERT (still FanOutOver-tagged, so the executor's dispatch guard never
// runs it, and it is marked completed).
//
// Append semantics (rather than in-place insertion + renumbering) is deliberate: the
// executor expands mid-run while sibling steps are in flight, and renumbering would race
// with their `step_N_result` writes and their completed/dispatched bookkeeping. Appending
// disturbs no existing index, so no remap is needed anywhere.
//
// Pure function of (plan, fanIdx, items). maxWidth <= 0 disables the cap; exceeding it
// returns *FanOutWidthError so the caller can replan instead of silently truncating. An
// empty item set drops the dependency on the node (discovery found nothing to do) so
// dependents proceed rather than stall.
func ExpandFanOut(plan *ExecutionPlan, fanIdx int, items []string, maxWidth int) (*ExecutionPlan, error) {
	if plan == nil {
		return nil, fmt.Errorf("fan-out: nil plan")
	}
	if fanIdx < 0 || fanIdx >= len(plan.Steps) {
		return nil, fmt.Errorf("fan-out: step index %d out of range (%d steps)", fanIdx, len(plan.Steps))
	}
	base := plan.Steps[fanIdx]
	if base.FanOutOver == nil {
		return nil, fmt.Errorf("fan-out: step %d is not parametric", fanIdx)
	}
	k := len(items)
	if maxWidth > 0 && k > maxWidth {
		return nil, &FanOutWidthError{StepIndex: fanIdx, Width: k, MaxWidth: maxWidth}
	}

	n := len(plan.Steps)
	childIdx := make([]int, k) // the appended children's indices: n, n+1, ...
	for c := range k {
		childIdx[c] = n + c
	}

	// rewriteDeps replaces any dependency on the parametric node with the child set.
	rewriteDeps := func(deps []int) []int {
		if len(deps) == 0 {
			return nil
		}
		out := make([]int, 0, len(deps)+k)
		seen := map[int]bool{}
		for _, d := range deps {
			if d == fanIdx {
				for _, c := range childIdx {
					if !seen[c] {
						seen[c] = true
						out = append(out, c)
					}
				}
				continue
			}
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
		return out
	}

	out := plan.Clone()             // deep copy — existing indices preserved
	out.PlanningFacts = plan.PlanningFacts // Clone drops these (json:"-"); keep them mid-run
	for i := range out.Steps {
		out.Steps[i].DependsOn = rewriteDeps(out.Steps[i].DependsOn)
	}
	// Append the children: the parametric node, once per item, made concrete.
	varTok := "{" + base.FanOutVarName() + "}"
	childDeps := append([]int(nil), base.DependsOn...) // the node's own deps (unchanged indices)
	for _, item := range items {
		c := base
		c.FanOutOver = nil // a child is concrete — never re-expands
		c.FanOutVar = ""
		c.Query = strings.ReplaceAll(base.Query, varTok, item)
		c.DependsOn = append([]int(nil), childDeps...)
		out.Steps = append(out.Steps, c)
	}
	return out, nil
}

// PendingFanOut reports the index of the first parametric step whose source step is
// srcIdx, or -1. The executor calls this when a step completes to decide whether the
// plan must expand before the run can continue.
func PendingFanOut(plan *ExecutionPlan, srcIdx int) int {
	if plan == nil {
		return -1
	}
	for i, s := range plan.Steps {
		if s.FanOutOver != nil && *s.FanOutOver == srcIdx {
			return i
		}
	}
	return -1
}
