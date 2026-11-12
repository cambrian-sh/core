package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
	// Content is the result, whole unless Shortened; then its BEGINNING.
	Content string `json:"content,omitempty"`
	// Shortened marks a result larger than its share of the budget: Content is its
	// beginning and Tail its end, OmittedFrom..OmittedTo the byte range left out,
	// Outline its headings with their pages, and Read the exact call that returns
	// the omitted pages (FitStepDependencies). The whole is readable by CID.
	Shortened bool `json:"shortened,omitempty"`
	// Tail is the END of a shortened result.
	Tail string `json:"tail,omitempty"`
	// OmittedFrom and OmittedTo are the byte offsets of the part of a shortened
	// result that is not shown.
	OmittedFrom int `json:"omitted_from,omitempty"`
	OmittedTo   int `json:"omitted_to,omitempty"`
	// PageBytes is the page size Outline and Read are numbered in.
	PageBytes int `json:"page_bytes,omitempty"`
	// Outline is the headings of the whole result with the page each is on.
	Outline []DependencyHeading `json:"outline,omitempty"`
	// Read is the exact call that returns the omitted pages.
	Read string `json:"read,omitempty"`
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

// DependencyFit is how a dependent step's upstream results are bounded
// (execution.content): BudgetBytes in total across the step's dependencies (<= 0 ⇒
// unbounded), and PageBytes, the ledger_get page size the outline and the read hint
// of a shortened result are numbered in (<= 0 ⇒ 16000).
type DependencyFit struct {
	BudgetBytes int
	PageBytes   int
}

// DependencyHeading is one heading of a shortened result's outline, with the
// ledger_get page it is on.
type DependencyHeading struct {
	Heading string `json:"heading"`
	Page    int    `json:"page"`
}

// Outline bounds: a shortened result's outline names at most this many headings,
// each cut to dependencyHeadingMax bytes, and takes at most a fifth of the result's
// share (an outline that crowds out the text it indexes defeats itself).
const (
	dependencyOutlineMax = 40
	dependencyHeadingMax = 100
)

// FitStepDependencies bounds the inlined content of deps to fit.BudgetBytes in total
// (owner decision 2026-11-12, T-20261112-summarise-a-document-step-gets-only-the-first).
//
// Every dependency keeps a fair share: the budget is split evenly, a result smaller
// than its share gives the rest back to the others (water-filling), and only results
// larger than the final share are shortened. A shortened result is NOT a head-only
// cut — that silently biased p12's summary to the start of the report and dropped its
// end-of-report finding. It keeps its BEGINNING (Content) and its END (Tail), each
// cut on a rune boundary, the byte range left out (OmittedFrom..OmittedTo), the
// outline of the whole result (its markdown headings with the ledger_get page each
// is on) and Read, the exact call that returns the omitted pages. deps is modified
// in place and returned. fit.BudgetBytes <= 0 leaves deps whole.
func FitStepDependencies(deps []StepDependency, fit DependencyFit) []StepDependency {
	budget := fit.BudgetBytes
	if budget <= 0 || len(deps) == 0 {
		return deps
	}
	page := fit.PageBytes
	if page <= 0 {
		page = 16000
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
		if len(deps[i].Content) > share {
			shortenDependency(&deps[i], share, page)
		}
	}
	return deps
}

// shortenDependency keeps the beginning and the end of d's content within share
// bytes, with the outline of the whole and the read hint for what is left out.
func shortenDependency(d *StepDependency, share, page int) {
	full := d.Content
	starts := TextPageStarts(full, page)
	outline := dependencyOutline(full, starts, share/5)
	cost := 0
	for _, h := range outline {
		cost += len(h.Heading) + 8
	}
	avail := max(share-cost, 0)
	head := RunePrefix(full, avail-avail/2)
	tail := RuneSuffix(full, avail/2)
	d.Content, d.Tail = head, tail
	d.Shortened = true
	d.OmittedFrom, d.OmittedTo = len(head), len(full)-len(tail)
	d.PageBytes = page
	d.Outline = outline
	if d.CID != "" && d.OmittedTo > d.OmittedFrom {
		a := PageOfOffset(starts, d.OmittedFrom)
		b := PageOfOffset(starts, d.OmittedTo-1)
		verb, arg := "resolve_cid", "cid"
		if IsLedgerID(d.CID) {
			verb, arg = "ledger_get", "id"
		}
		if a == b {
			d.Read = fmt.Sprintf("%s %s='%s' page=%d", verb, arg, d.CID, a)
		} else {
			d.Read = fmt.Sprintf("%s %s='%s' page=%d (then page=%d … page=%d)", verb, arg, d.CID, a, a+1, b)
		}
	}
}

// dependencyOutline is the markdown (ATX) headings of text with the page each
// starts on, bounded to dependencyOutlineMax entries and maxBytes in total.
func dependencyOutline(text string, starts []int, maxBytes int) []DependencyHeading {
	var out []DependencyHeading
	used, off := 0, 0
	for _, line := range strings.SplitAfter(text, "\n") {
		at := off
		off += len(line)
		t := strings.TrimSpace(line)
		level := len(t) - len(strings.TrimLeft(t, "#"))
		if level == 0 || level > 6 || len(t) == level || t[level] != ' ' {
			continue
		}
		h := RunePrefix(t, dependencyHeadingMax)
		if len(out) >= dependencyOutlineMax || used+len(h)+8 > maxBytes {
			break
		}
		used += len(h) + 8
		out = append(out, DependencyHeading{Heading: h, Page: PageOfOffset(starts, at)})
	}
	return out
}
