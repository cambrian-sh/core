package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ─────────────────────────────────────────────────────────────────────────────
// Plan action ledger (T-20261107-plan-result-is-only-the-last-topo-step-a-plan).
//
// A delegated plan's caller — the chat agent above all — must learn from the plan's
// result what the plan DID, not only what its last step said. The step results are
// the agents' own prose; the kernel itself sees every tool call at ExecuteTool, so it
// keeps a deterministic record of the side-effecting ones per run and appends it to
// the result. A chat agent that was told "OPS-14 was created" and nothing about
// OPS-11..13 re-delegated those three and created them twice.
// ─────────────────────────────────────────────────────────────────────────────

// Plan action statuses.
const (
	// PlanActionDone: the tool ran and returned a result.
	PlanActionDone = "done"
	// PlanActionFailed: the tool ran (or was attempted) and returned an error; whether
	// the effect landed is unknown — check before retrying.
	PlanActionFailed = "failed"
	// PlanActionRefused: the kernel refused the call (no grant, approval declined, a
	// hook, a policy); it did not run.
	PlanActionRefused = "refused"
)

// PlanAction is one side-effecting tool call a plan made, as the kernel recorded it.
type PlanAction struct {
	// Step is the plan step that made the call; -1 when not step-scoped.
	Step int
	// Agent is the calling agent.
	Agent string
	// Tool is the tool's full name (mcp:<server>/<tool>, local:<machine>/<tool>, ...).
	Tool string
	// Args is a redacted, bounded summary of the call's arguments (RedactArgs).
	Args map[string]string
	// IDs are the identifiers the tool returned (PlanActionIDs), "field=value".
	IDs []string
	// Status is one of the PlanAction* constants.
	Status string
	// Detail is the error or refusal reason; "" for a done call.
	Detail string
	// ApprovalState is the approval resolution of a call refused at the approval
	// gate (ToolCallResponse.ApprovalState); "" otherwise. Not rendered.
	ApprovalState ApprovalState
	// Generation is the plan generation of the step (LeaseBinding.PlanGeneration):
	// step indices restart after a replan, so a step's own calls are (Generation,
	// Step). Not rendered.
	Generation int
	// Target is the write's target as the ADR-0145 D14 write record identifies it
	// ("<tool>|<field>=<value>", WriteTargets.Key); "" when none. Not rendered.
	Target string
}

// planActionIDKeys are result fields that name what a write created or touched, in
// preference order; any other field ending in "_id"/"Id" counts too.
var planActionIDKeys = []string{"id", "key", "number", "message_id", "messageId", "thread_id", "url", "path"}

// maxPlanActionIDs bounds the identifiers kept per call.
const maxPlanActionIDs = 4

// PlanActionIDs extracts the identifiers a tool result names, deterministically: the
// result's top-level scalar fields that are ids (planActionIDKeys, or *_id / *Id), and
// — when the top level has none — the same fields one object down ({"issue":{"key":..}}).
// It understands the MCP shapes the kernel stores: a structured object, or a content
// array whose text items hold JSON. Anything else yields nil; the action is still
// listed, just without ids.
func PlanActionIDs(result []byte) []string {
	obj := resultObject(result)
	if obj == nil {
		return nil
	}
	if ids := idFields(obj, ""); len(ids) > 0 {
		return ids
	}
	keys := sortedKeys(obj)
	var out []string
	for _, k := range keys {
		if inner, ok := obj[k].(map[string]any); ok {
			out = append(out, idFields(inner, k+".")...)
		}
		if len(out) >= maxPlanActionIDs {
			return out[:maxPlanActionIDs]
		}
	}
	return out
}

// resultObject decodes a tool result into the object that carries its fields.
func resultObject(result []byte) map[string]any {
	var v any
	if json.Unmarshal(result, &v) != nil {
		return nil
	}
	switch t := v.(type) {
	case map[string]any:
		// {"result": {...}} / {"structuredContent": {...}} wrappers.
		for _, k := range []string{"structuredContent", "result"} {
			if inner, ok := t[k].(map[string]any); ok {
				return inner
			}
		}
		return t
	case []any:
		// MCP content array: the first text item that is a JSON object.
		for _, e := range t {
			item, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := item["text"].(string); ok {
				var inner map[string]any
				if json.Unmarshal([]byte(strings.TrimSpace(text)), &inner) == nil {
					return inner
				}
			}
		}
	}
	return nil
}

func idFields(obj map[string]any, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if seen[k] {
			return
		}
		v, ok := scalarString(obj[k])
		if !ok || v == "" {
			return
		}
		seen[k] = true
		out = append(out, prefix+k+"="+boundRunes(v, 120))
	}
	for _, k := range planActionIDKeys {
		if _, present := obj[k]; present {
			add(k)
		}
	}
	for _, k := range sortedKeys(obj) {
		if strings.HasSuffix(k, "_id") || (strings.HasSuffix(k, "Id") && len(k) > 2) {
			add(k)
		}
	}
	if len(out) > maxPlanActionIDs {
		out = out[:maxPlanActionIDs]
	}
	return out
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%v", t), ".0"), true
	case bool:
		return fmt.Sprint(t), true
	}
	return "", false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// boundRunes cuts s to at most n runes, marking a cut with "…". Never splits a rune.
func boundRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// planActionArgRunes bounds one argument value in the rendered ledger; planActionMaxArgs
// bounds how many arguments a line shows.
const (
	planActionArgRunes = 80
	planActionMaxArgs  = 6
)

// PlanActionsHeader opens the rendered ledger. It is worded for the reader that
// matters most — a chat agent deciding what is left to do — so the "already
// happened" fact is in the data itself and needs no prompt rule to be understood.
const PlanActionsHeader = "COMPLETED ACTIONS — the kernel's record of every side-effecting tool call this plan made. " +
	"Calls marked done already happened: do not repeat them. A failed call may or may not have taken effect; check before retrying."

// RenderPlanActions renders the ledger as a numbered, deterministic block, or "" when
// there is nothing to report. Order is the order the kernel recorded the calls.
func RenderPlanActions(actions []PlanAction) string {
	if len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(PlanActionsHeader)
	for i, a := range actions {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%d. ", i+1)
		if a.Step >= 0 {
			fmt.Fprintf(&b, "step %d · ", a.Step)
		}
		b.WriteString(a.Tool)
		b.WriteString(" · ")
		b.WriteString(a.Status)
		if a.Detail != "" {
			b.WriteString(" (" + boundRunes(oneLine(a.Detail), 160) + ")")
		}
		if len(a.IDs) > 0 {
			b.WriteString(" · ids: " + strings.Join(a.IDs, ", "))
		}
		if len(a.Args) > 0 {
			keys := make([]string, 0, len(a.Args))
			for k := range a.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for j, k := range keys {
				if j == planActionMaxArgs {
					parts = append(parts, fmt.Sprintf("+%d more", len(keys)-j))
					break
				}
				parts = append(parts, k+"="+boundRunes(oneLine(a.Args[k]), planActionArgRunes))
			}
			b.WriteString(" · args: " + strings.Join(parts, "; "))
		}
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
