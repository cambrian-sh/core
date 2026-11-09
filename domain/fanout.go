package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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
// identity when a source step emits an array of objects rather than of strings. A row
// keyed by none of them is still an item (see objectItem): it is never dropped.
var fanOutItemKeys = []string{"id", "item", "name", "path", "title"}

// fanOutArrayKeys are the object fields, in preference order, that carry the set when a
// source step emits a wrapper object rather than a bare array.
var fanOutArrayKeys = []string{"items", "entities", "missing", "results", "files", "list", "data"}

// fanOutWrapperDepth bounds how far a wrapper object is descended ({"data":{"items":[…]}}).
const fanOutWrapperDepth = 3

// fanOutScanLimit and fanOutScanAttempts bound the JSON scan of a source output: past
// this size, or this many failed decode attempts, the scan stops (what was not reached
// is treated as unrecognised). Source outputs are a few KB; the bound only keeps a
// pathological reply from costing a decode per bracket.
const (
	fanOutScanLimit    = 256 << 10
	fanOutScanAttempts = 256
)

// ExtractFanOutItems deterministically pulls the item set out of a source step's output
// (ADR-0051 D10: "extracted deterministically, no LLM re-parse"). It accepts, in order:
// a structured list (see ExtractStructuredFanOutItems); and otherwise, for FREE TEXT
// only, falls back to the output's LIST lines — markdown structure (headings, table
// header and separator rows, rules, code fences, lead-in lines ending in ':') is dropped
// and list bullets are stripped. Duplicates are dropped, order is preserved, so
// expansion is stable for the same output.
//
// Output that holds JSON it does not recognise as a list (SourceHoldsJSON) is NEVER
// split into lines: it returns nil. A JSON object with two arrays followed by the item
// array was split into 26 children of "{", "],", "\"contact_email\": …" for three
// customers (T-20261107-read-only-fan-out-over-an-unrecognised-json); ResolveFanOut
// demotes that case to one step instead.
//
// The line fallback is only ever used for a READ-ONLY fan-out (ResolveFanOut): free
// text cannot be split into items reliably, and a side-effecting fan-out over a
// mis-split source repeats its write once per junk line
// (T-20261107-fan-out-over-a-prose-source-splits-markdown).
func ExtractFanOutItems(raw string) []string {
	if items, ok := ExtractStructuredFanOutItems(raw); ok {
		return items
	}
	if SourceHoldsJSON(raw) {
		return nil
	}
	return listLines(raw)
}

// listLines is the free-text fallback: the list lines of raw, deduplicated.
func listLines(raw string) []string {
	all := strings.Split(strings.TrimSpace(raw), "\n")
	var lines []string
	for i, ln := range all {
		if i+1 < len(all) && isTableSeparator(all[i+1]) {
			continue // a table's header row: the row above its |---| separator
		}
		if t, ok := listLineItem(ln); ok {
			lines = append(lines, t)
		}
	}
	return dedupe(lines)
}

// ExtractStructuredFanOutItems returns the item set when the source output carries a
// structured list, and ok=false otherwise. A list is a JSON array (of strings, numbers,
// or objects — see objectItem) or a JSON object wrapping one: under a known key
// (fanOutArrayKeys), as its single array-valued field, or as the single object-valued
// field of a wrapper ({"data":{"customers":[…]}}, up to fanOutWrapperDepth). An object
// with several array fields and no known key is not a list — refuse to guess which.
//
// Where the JSON may sit: the whole output; or a JSON value that starts a line — bare,
// or inside a ``` fenced block — in a reply that also holds prose or other JSON. A
// source step is told to END its reply with the item array (prompts/fanout_source), so:
//
//   - when the reply ENDS with a list (only whitespace or a closing fence after it),
//     that list is the item set, whatever JSON came before it. The o3 #1 source of
//     T-20261107-read-only-fan-out-over-an-unrecognised-json answered with a JSON
//     object holding two arrays, a blank line, then the requested array of three names;
//   - otherwise, exactly one list anywhere in the reply is the item set;
//   - otherwise (none, or several with none at the end) ok=false.
//
// Free text, markdown lists and tables are NOT structured: their lines are prose a
// fan-out cannot tell from items.
func ExtractStructuredFanOutItems(raw string) ([]string, bool) {
	arr, ok := structuredFanOutArray(raw)
	if !ok {
		return nil, false
	}
	return dedupe(itemsFromArray(arr)), true
}

// structuredFanOutArray is the element list ExtractStructuredFanOutItems derives its
// items from (same selection rules), kept whole so a child can receive its item
// OBJECT, not only the label the item is named by (FanOutItemObjects).
func structuredFanOutArray(raw string) ([]any, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false
	}
	var whole any
	if json.Unmarshal([]byte(s), &whole) == nil {
		return arrayFromJSON(whole, 0)
	}
	vals := scanJSONValues(s)
	if n := len(vals); n > 0 && vals[n-1].atEnd {
		if arr, ok := arrayFromJSON(vals[n-1].val, 0); ok {
			return arr, true
		}
	}
	var lists [][]any
	for _, v := range vals {
		if arr, ok := arrayFromJSON(v.val, 0); ok {
			lists = append(lists, arr)
		}
	}
	if len(lists) == 1 {
		return lists[0], true
	}
	return nil, false
}

// FanOutItemObjects maps each item label of a structured source output to the
// whole element it was taken from, as compact JSON (ADR-0145 D6.3: a fan-out child
// receives its whole item object — the record's amount, date and id, not only the
// name it is labelled by). nil when the source holds no structured list. The first
// element of a repeated label wins, matching dedupe.
func FanOutItemObjects(raw string) map[string]string {
	arr, ok := structuredFanOutArray(raw)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(arr))
	for _, e := range arr {
		labels := itemsFromArray([]any{e})
		if len(labels) != 1 {
			continue
		}
		if _, seen := out[labels[0]]; seen {
			continue
		}
		if b, err := json.Marshal(e); err == nil {
			out[labels[0]] = string(b)
		}
	}
	return out
}

// SourceHoldsJSON reports whether a fan-out source output carries JSON: a decodable
// JSON object or array starting a line, or at least two JSON-shaped lines ("key":
// value, a lone brace or bracket) of a value that was cut short. Such output must
// never be split into lines — each line is a fragment of one record, not an item.
func SourceHoldsJSON(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if len(scanJSONValues(s)) > 0 {
		return true
	}
	frag := 0
	for _, ln := range strings.Split(s, "\n") {
		if jsonFragmentLine(strings.TrimSpace(ln)) {
			frag++
		}
	}
	return frag >= 2
}

// jsonFragmentLine reports a line that only occurs inside pretty-printed JSON: a lone
// brace or bracket, or a `"key": …` member line.
func jsonFragmentLine(t string) bool {
	switch strings.TrimRight(t, ",") {
	case "{", "}", "[", "]", "{}", "[]":
		return true
	}
	if !strings.HasPrefix(t, `"`) {
		return false
	}
	end := strings.IndexByte(t[1:], '"')
	return end >= 0 && strings.HasPrefix(strings.TrimSpace(t[end+2:]), ":")
}

// jsonValue is one JSON object or array found in a source output.
type jsonValue struct {
	val   any
	atEnd bool // only whitespace and closing fences follow it
}

// scanJSONValues returns, in order, the JSON objects and arrays that start a line of s
// (after indentation), including those inside fenced code blocks: a fence line is not
// JSON, so the scan steps over it to the body. A decoded value is skipped whole, so its
// nested arrays are never reported separately. Line-start only: a bracket inside a
// sentence ("see [1]") is not taken for a list.
func scanJSONValues(s string) []jsonValue {
	if len(s) > fanOutScanLimit {
		s = s[:fanOutScanLimit]
	}
	var out []jsonValue
	attempts := 0
	for i := 0; i < len(s) && attempts < fanOutScanAttempts; {
		c := s[i]
		if (c != '{' && c != '[') || !atLineStart(s, i) {
			i++
			continue
		}
		attempts++
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v any
		if err := dec.Decode(&v); err != nil {
			i++
			continue
		}
		end := i + int(dec.InputOffset())
		out = append(out, jsonValue{val: v, atEnd: onlyClosingAfter(s[end:])})
		i = end
	}
	return out
}

// atLineStart reports whether s[i] is the first non-blank character of its line.
func atLineStart(s string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch s[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

// onlyClosingAfter reports whether rest holds nothing but whitespace and ``` lines.
func onlyClosingAfter(rest string) bool {
	for _, ln := range strings.Split(rest, "\n") {
		if t := strings.TrimSpace(ln); t != "" && t != "```" {
			return false
		}
	}
	return true
}

// listLineItem reads one line of a free-text source as a list item. Markdown
// structure is not an item: headings, table rows of dashes (separators) and the
// table's header row are dropped, as are rules, fences and lead-in lines ending in
// ':'. A bullet or ordinal prefix is stripped. A table BODY row is kept whole.
func listLineItem(ln string) (string, bool) {
	t := strings.TrimSpace(ln)
	switch {
	case t == "",
		strings.HasPrefix(t, "#"),
		strings.HasPrefix(t, "```"),
		strings.HasSuffix(t, ":"),
		strings.Trim(t, "-*_= ") == "",
		isTableSeparator(t):
		return "", false
	}
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, p) {
			t = strings.TrimSpace(t[len(p):])
			break
		}
	}
	if i := strings.IndexAny(t, ".)"); i > 0 && i <= 3 && isDigits(t[:i]) && len(t) > i+1 && t[i+1] == ' ' {
		t = strings.TrimSpace(t[i+2:])
	}
	return t, t != ""
}

// isTableSeparator reports a markdown table separator row (|---|:--:|).
func isTableSeparator(ln string) bool {
	t := strings.TrimSpace(ln)
	return strings.HasPrefix(t, "|") && strings.Contains(t, "-") && strings.Trim(t, "|-: ") == ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// listFromJSON reads a decoded JSON value as an item list; ok=false means "not a list
// we understand". depth counts the wrapper objects already descended.
func listFromJSON(v any, depth int) ([]string, bool) {
	arr, ok := arrayFromJSON(v, depth)
	if !ok {
		return nil, false
	}
	return itemsFromArray(arr), true
}

// arrayFromJSON is listFromJSON before the elements become item labels.
func arrayFromJSON(v any, depth int) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case map[string]any:
		if depth >= fanOutWrapperDepth {
			return nil, false
		}
		for _, k := range fanOutArrayKeys {
			if arr, ok := t[k].([]any); ok {
				return arr, true
			}
		}
		// Exactly one array-valued field ⇒ unambiguous; more than one ⇒ refuse to guess.
		var arr []any
		var obj map[string]any
		arrays, objects := 0, 0
		for _, fv := range t {
			switch x := fv.(type) {
			case []any:
				arr, arrays = x, arrays+1
			case map[string]any:
				obj, objects = x, objects+1
			}
		}
		switch {
		case arrays == 1:
			return arr, true
		case arrays == 0 && objects == 1:
			return arrayFromJSON(obj, depth+1) // a wrapper one level down
		}
		return nil, false
	}
	return nil, false
}

// itemsFromArray turns array elements into items. Every element that carries a value
// becomes an item — an object row is never silently dropped (objectItem): an array of
// three records is three children. It used to be zero when the rows were keyed by,
// say, customer_name, which is none of fanOutItemKeys.
func itemsFromArray(arr []any) []string {
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		switch v := e.(type) {
		case string:
			if t := strings.TrimSpace(v); t != "" {
				out = append(out, t)
			}
		case float64:
			out = append(out, strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(v))
		case map[string]any:
			if t := objectItem(v); t != "" {
				out = append(out, t)
			}
		case []any:
			if len(v) > 0 {
				if b, err := json.Marshal(v); err == nil {
					out = append(out, string(b))
				}
			}
		}
	}
	return out
}

// objectItem is the item a JSON object row stands for: the first non-empty string
// field among fanOutItemKeys; else the first (by key order) string field whose key ends
// in "name" ("customer_name"), then in "id" ("account_id"); else the whole row as
// compact JSON (map keys marshal sorted, so it is deterministic) — the child then sees
// the full record, which beats the row vanishing.
func objectItem(obj map[string]any) string {
	str := func(k string) string {
		if sv, ok := obj[k].(string); ok {
			return strings.TrimSpace(sv)
		}
		return ""
	}
	for _, k := range fanOutItemKeys {
		if t := str(k); t != "" {
			return t
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, suffix := range []string{"name", "id"} {
		for _, k := range keys {
			if strings.HasSuffix(strings.ToLower(k), suffix) {
				if t := str(k); t != "" {
					return t
				}
			}
		}
	}
	if len(obj) == 0 {
		return ""
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
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

// FanOutTemplated reports whether a parametric step's query contains its fan-out
// variable token ("{item}", or "{<FanOutVar>}"). Only a templated step can be split
// per item: substitution is the ONLY thing that makes one child differ from another.
func (s Step) FanOutTemplated() bool {
	return strings.Contains(s.Query, "{"+s.FanOutVarName()+"}")
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
// empty item set KEEPS the dependency on the inert node: the executor marks the node
// completed (so dependents proceed rather than stall) and records its result as empty,
// so a dependent is told "this fan-out produced nothing" (domain.StepDependencyEmpty)
// instead of the dependency silently vanishing from its inputs
// (T-20261105-a-planner-thought-step-that-depends-on-two).
//
// An UNTEMPLATED parametric step — FanOutOver set but no {<var>} token in its query —
// is DEMOTED instead of expanded: the returned plan has that step as an ordinary,
// concrete step (FanOutOver cleared, the source added to its DependsOn) that runs ONCE
// over the whole set, and nothing is appended. Substituting into a template with no
// token is a no-op, so expansion would make N byte-identical children, each carrying
// the planner's whole-set instruction — a side-effecting step then repeats every write
// N times (15 OPS issues for 3 incidents, T-20261107-fan-out-step-without-an-item-
// placeholder). Running it once is what the instruction itself asks for. The caller
// tells the two outcomes apart by out.Steps[fanIdx].FanOutOver: nil ⇒ demoted (dispatch
// it), non-nil ⇒ expanded (the node is inert; mark it completed). The width cap does not
// apply to a demoted step — it is one step.
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
	if !base.FanOutTemplated() {
		return demoteFanOut(plan, fanIdx), nil
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
		if k == 0 {
			return append([]int(nil), deps...) // keep: the empty node is reported, not dropped
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

	out := plan.Clone()                    // deep copy — existing indices preserved
	out.PlanningFacts = plan.PlanningFacts // Clone drops these (json:"-"); keep them mid-run
	for i := range out.Steps {
		out.Steps[i].DependsOn = rewriteDeps(out.Steps[i].DependsOn)
	}
	// Append the children: the parametric node, once per item, made concrete.
	varTok := "{" + base.FanOutVarName() + "}"
	// A child depends on the node's own deps AND on the fan-out source (ADR-0145
	// D6.3): the source's output is what the child's item came from, so the child
	// receives it like any other upstream result — it used to inherit only the
	// node's own DependsOn and never saw the record its item names.
	childDeps := append([]int(nil), base.DependsOn...) // the node's own deps (unchanged indices)
	if src := *base.FanOutOver; src >= 0 && !slices.Contains(childDeps, src) {
		childDeps = append(childDeps, src)
	}
	for _, item := range items {
		c := base
		c.FanOutOver = nil // a child is concrete — never re-expands
		c.FanOutVar = ""
		c.Query = strings.ReplaceAll(base.Query, varTok, item) // templated: checked above
		c.FanOutItem = item
		// The whole item; a scalar item is its own object. ResolveFanOut replaces
		// it with the source element the label was taken from (FanOutItemObjects).
		if b, err := json.Marshal(item); err == nil {
			c.FanOutItemJSON = string(b)
		}
		c.DependsOn = append([]int(nil), childDeps...)
		out.Steps = append(out.Steps, c)
	}
	return out, nil
}

// demoteFanOut returns plan with the parametric step at fanIdx made an ordinary,
// concrete step that runs ONCE over the whole source set: FanOutOver/FanOutVar cleared
// and the source added to its DependsOn (the source's output is its input; only a
// dependency hands it over). A templated query has its {<var>} token replaced by a
// reference to the source's list, plus the exactly-once instruction, so the one step
// is told to cover every item and none twice. Nothing is appended.
func demoteFanOut(plan *ExecutionPlan, fanIdx int) *ExecutionPlan {
	out := plan.Clone()
	out.PlanningFacts = plan.PlanningFacts // Clone drops these (json:"-"); keep them mid-run
	st := &out.Steps[fanIdx]
	src := *st.FanOutOver
	if tok := "{" + st.FanOutVarName() + "}"; strings.Contains(st.Query, tok) {
		q := strings.TrimSpace(strings.ReplaceAll(st.Query, tok, fmt.Sprintf("(each item listed in the result of step %d)", src)))
		if !strings.HasSuffix(q, ".") && !strings.HasSuffix(q, "!") && !strings.HasSuffix(q, "?") {
			q += "."
		}
		st.Query = q + " Do this once for every item in that list, and never twice for the same item."
	}
	st.FanOutOver = nil
	st.FanOutVar = ""
	if !slices.Contains(st.DependsOn, src) {
		st.DependsOn = append(st.DependsOn, src)
	}
	return out
}

// FanOutSideEffecting reports whether a parametric step may change the world, so its
// children must only ever be split from a STRUCTURED list. A thought step is a model
// call with no tools; otherwise any declared effect other than "read" counts, and a
// step that declared nothing (RequiredEffects nil: a legacy or authored plan) counts
// too — unknown is treated as side-effecting, because the cost of being wrong is a
// write repeated once per junk item, and the cost of the safe answer is only that the
// step runs once over the whole set.
func (s Step) FanOutSideEffecting() bool {
	if s.IsThought {
		return false
	}
	if s.RequiredEffects == nil {
		return true
	}
	for _, e := range s.RequiredEffects {
		if strings.ToLower(strings.TrimSpace(e)) != "read" {
			return true
		}
	}
	return false
}

// FanOutOutcome is how ResolveFanOut handled one parametric step.
type FanOutOutcome int

const (
	// FanOutExpanded: one child per item was appended (possibly zero children).
	FanOutExpanded FanOutOutcome = iota
	// FanOutDemotedUntemplated: the query has no {<var>} token; the step runs once.
	FanOutDemotedUntemplated
	// FanOutDemotedUnstructured: a side-effecting step over a source that is not a
	// structured list; the step runs once over the whole source.
	FanOutDemotedUnstructured
	// FanOutDemotedUnrecognisedJSON: a read-only step over a source that holds JSON
	// but no recognisable list (SourceHoldsJSON); the step runs once over the whole
	// source rather than once per line of the JSON.
	FanOutDemotedUnrecognisedJSON
)

// ResolveFanOut is the one decision the executor makes when a fan-out source
// completes: expand the parametric step at fanIdx over srcOutput, or demote it to one
// ordinary step. The returned items are what the expansion used (nil when demoted).
//
//   - Untemplated query (no {<var>}): demoted — expansion could only make identical
//     children (T-20261107-fan-out-step-without-an-item-placeholder).
//   - Side-effecting step (FanOutSideEffecting) over a source that is NOT a structured
//     list (ExtractStructuredFanOutItems): demoted. Splitting free text into lines made
//     every markdown heading, table header and lead-in sentence a child, and each such
//     write child did the whole job — 54 issues for 3 incidents
//     (T-20261107-fan-out-over-a-prose-source-splits-markdown). Demotion is chosen over
//     asking the source for structured output: it is deterministic, needs no extra
//     model call, and guarantees the write instruction is executed by exactly one step.
//     An EMPTY source is not demoted: it expands to no children, as before.
//   - Read-only step over a source that holds JSON but no list the extractor
//     recognises (SourceHoldsJSON): demoted too (FanOutDemotedUnrecognisedJSON). A
//     line split of JSON makes every brace and member line a child — 26 children for
//     3 customers, ~3 minutes of junk lookups that ran the plan into its caller's
//     deadline (T-20261107-read-only-fan-out-over-an-unrecognised-json). One step over
//     the whole JSON reads it correctly; no line of it is ever an item.
//   - Otherwise: ExpandFanOut over the extracted items (structured, or for a
//     read-only step the list lines of free text). Over-width returns
//     *FanOutWidthError.
func ResolveFanOut(plan *ExecutionPlan, fanIdx int, srcOutput string, maxWidth int) (*ExecutionPlan, []string, FanOutOutcome, error) {
	if plan == nil {
		return nil, nil, FanOutExpanded, fmt.Errorf("fan-out: nil plan")
	}
	if fanIdx < 0 || fanIdx >= len(plan.Steps) {
		return nil, nil, FanOutExpanded, fmt.Errorf("fan-out: step index %d out of range (%d steps)", fanIdx, len(plan.Steps))
	}
	base := plan.Steps[fanIdx]
	if base.FanOutOver == nil {
		return nil, nil, FanOutExpanded, fmt.Errorf("fan-out: step %d is not parametric", fanIdx)
	}
	if !base.FanOutTemplated() {
		return demoteFanOut(plan, fanIdx), nil, FanOutDemotedUntemplated, nil
	}
	var items []string
	if base.FanOutSideEffecting() {
		structured, ok := ExtractStructuredFanOutItems(srcOutput)
		if !ok && strings.TrimSpace(srcOutput) != "" {
			return demoteFanOut(plan, fanIdx), nil, FanOutDemotedUnstructured, nil
		}
		items = structured
	} else if structured, ok := ExtractStructuredFanOutItems(srcOutput); ok {
		items = structured
	} else if SourceHoldsJSON(srcOutput) {
		return demoteFanOut(plan, fanIdx), nil, FanOutDemotedUnrecognisedJSON, nil
	} else {
		items = listLines(srcOutput)
	}
	out, err := ExpandFanOut(plan, fanIdx, items, maxWidth)
	if err != nil {
		return nil, nil, FanOutExpanded, err
	}
	// Each child receives the whole element its item was taken from (ADR-0145 D6.3).
	if objs := FanOutItemObjects(srcOutput); len(objs) > 0 {
		for i := len(plan.Steps); i < len(out.Steps); i++ {
			if o, ok := objs[out.Steps[i].FanOutItem]; ok {
				out.Steps[i].FanOutItemJSON = o
			}
		}
	}
	return out, items, FanOutExpanded, nil
}

// PendingFanOuts reports the indices of EVERY parametric step whose source step is
// srcIdx, in plan order (nil when there are none). The executor calls this when a step
// completes and expands each one — several parametric steps may fan out over the same
// source ("for each customer {item}: look up the contact" and "...: list the invoices"),
// and expanding only the first left its siblings, and every step depending on them,
// never dispatched (T-20261107-only-the-first-fan-out-step-over-a-source). An inert,
// already-expanded node still carries FanOutOver, so the caller skips completed indices.
func PendingFanOuts(plan *ExecutionPlan, srcIdx int) []int {
	if plan == nil {
		return nil
	}
	var out []int
	for i, s := range plan.Steps {
		if s.FanOutOver != nil && *s.FanOutOver == srcIdx {
			out = append(out, i)
		}
	}
	return out
}
