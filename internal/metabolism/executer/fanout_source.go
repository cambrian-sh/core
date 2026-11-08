package executer

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// fanOutSourcePromptID is the library fragment appended to a fan-out source
// step's task (prompts/fanout_source).
const fanOutSourcePromptID = "fanout_source"

// fanOutSourceTags is the data region of fanout_source; a closing tag inside a
// follow-up query is neutralised so the query cannot end its region early.
var fanOutSourceTags = []string{"follow_up"}

// fanOutSourceQuery is the task a step is dispatched with: its own query, plus —
// when a templated fan_out_over step reads its items from this step — the
// instruction to end the reply with the items as one fenced JSON array
// (T-20261107-planner-prompt-does-not-steer-fan-out-no-rule).
//
// Why the executor says it, not only the planner prompt: domain.ResolveFanOut
// splits a side-effecting fan-out only over a STRUCTURED list and demotes it to
// one step over anything else, so a source that answered in plain lines turned a
// parallel write batch into one sequential step. The planner prompt asks for a
// JSON-array source step, but a prompt is a request; this is deterministic — the
// executor knows the dependency at dispatch time, whatever the planner wrote, and
// it covers replanned and operator-authored plans too. The read-only fan-out gets
// it as well: its line fallback cannot tell headings and lead-ins from items.
//
// An untemplated parametric step is demoted whatever the source says, so it asks
// for nothing. A render failure (a broken embedded prompt) logs and sends the
// query unchanged: the fan-out then falls back to its documented demotion.
func fanOutSourceQuery(plan *domain.ExecutionPlan, idx int) string {
	query := plan.Steps[idx].Query
	var followUps []string
	for _, fi := range domain.PendingFanOuts(plan, idx) {
		st := plan.Steps[fi]
		if fi == idx || !st.FanOutTemplated() {
			continue
		}
		followUps = append(followUps, fmt.Sprintf("%s\n(%s stands for one item)",
			strings.TrimSpace(st.Query), "{"+st.FanOutVarName()+"}"))
	}
	if len(followUps) == 0 {
		return query
	}
	p, err := prompts.Get(fanOutSourcePromptID)
	if err != nil {
		slog.Warn("DAGExecutor: fan-out source instruction unavailable; dispatching the step's query alone",
			"step", idx, "err", err)
		return query
	}
	r, err := p.Render(map[string]string{
		"follow_up": prompts.NeutraliseClosingTags(strings.Join(followUps, "\n\n"), fanOutSourceTags...),
	})
	if err != nil {
		slog.Warn("DAGExecutor: fan-out source instruction did not render; dispatching the step's query alone",
			"step", idx, "err", err)
		return query
	}
	return strings.TrimRight(query, "\n") + "\n\n" + r.Text
}

// fanOutLogHead bounds what a fan-out log line quotes from a source or its items.
const (
	fanOutLogHeadChars = 200
	fanOutLogHeadItems = 5
)

// fanOutSourceHead is the start of a fan-out source output, for a log line.
func fanOutSourceHead(src string) string {
	r := []rune(strings.TrimSpace(src))
	if len(r) > fanOutLogHeadChars {
		return string(r[:fanOutLogHeadChars]) + "…"
	}
	return string(r)
}

// fanOutItemsHead is the first few expanded items, each shortened, for a log line.
func fanOutItemsHead(items []string) []string {
	n := min(len(items), fanOutLogHeadItems)
	out := make([]string, 0, n)
	for _, it := range items[:n] {
		if r := []rune(it); len(r) > 80 {
			it = string(r[:80]) + "…"
		}
		out = append(out, it)
	}
	return out
}
