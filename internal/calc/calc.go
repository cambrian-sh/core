// Package calc is the ADR-0145 D4 calculation tools: deterministic, kernel-served
// tools (domain.KernelTool, like ledger_get / ledger_find) for the exact work no
// model may do in prose — dates, working days, totals, counts, group-bys and
// threshold or rule comparisons (T-20261108-adr-0145-d4-kernel-calculation-tools-working).
//
// Why kernel tools: every agent reaches them through the one ExecuteTool path
// (decisions/all-agent-communication-passes-through-the-kernel.md), with the
// caller's lease-resolved provenance and read predicate; they need no grant and
// are on every menu (they change nothing in the world), so a compute or synthesis
// step has them. Large inputs
// are passed BY REFERENCE (a ledger id plus a JSON path), so bulk data never goes
// through a model to be computed on; references are read through the ENFORCED
// ledger reader under the caller's predicate (AGENTS.md §2.4), so a calculation
// can never read what its caller could not.
//
// Every result is recorded as a `computed` ledger entry carrying its inputs (the
// arguments, the referenced ids, the result), derived from the referenced entries
// so it is never easier to read than they are. Its id is returned as ledger_ref:
// a worker cites it as the evidence of a figure (ADR-0145 D6: a figure must come
// from a tool result or a calculation, never model text).
//
// Wired by app/calc_tools.go under execution.tools.calc_tools (default off).
package calc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Tool names. Reserved: a registry tool of the same name is never reached
// (kernel tools resolve first).
const (
	ToolDateWindow  = "calc_date_window"
	ToolDateAdd     = "calc_date_add"
	ToolWorkingDays = "calc_working_days"
	ToolAggregate   = "calc_aggregate"
	ToolCompare     = "calc_compare"
)

// Names lists every calculation tool.
var Names = []string{ToolDateWindow, ToolDateAdd, ToolWorkingDays, ToolAggregate, ToolCompare}

// Deps are what the tools compute over.
type Deps struct {
	// Calendar is the kernel's one "today" (domain.Calendar; nil ⇒ UTC wall clock).
	Calendar *domain.Calendar
	// Work is the organisation's working-day calendar.
	Work WorkCalendar
	// Reader is the ENFORCED ledger reader references are read through; nil ⇒
	// references are refused (the run ledger is off).
	Reader domain.LedgerReader
	// Writer records each result as a computed entry; nil ⇒ results are returned
	// but not recorded.
	Writer domain.LedgerWriter
	// MaxRows bounds the rows one aggregate reads (0 = 100000) and MaxListed the
	// rows an op=list result returns (0 = 500).
	MaxRows   int
	MaxListed int
}

// Tools returns the calculation tools over d.
func Tools(d Deps) []domain.KernelTool {
	if d.MaxRows <= 0 {
		d.MaxRows = 100000
	}
	if d.MaxListed <= 0 {
		d.MaxListed = 500
	}
	c := &core{d: d}
	return []domain.KernelTool{dateWindowTool{c}, dateAddTool{c}, workingDaysTool{c}, aggregateTool{c}, compareTool{c}}
}

type core struct{ d Deps }

// today is the calendar's date at midnight UTC (calc works on calendar days).
func (c *core) today() time.Time {
	t := c.d.Calendar.Now()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// finish records a result and returns the tool's JSON answer: the result plus
// ledger_ref (when recorded). inputs are the call's arguments; refs the ledger
// entries it read.
func (c *core) finish(ctx context.Context, call domain.KernelToolCall, tool string, refs []string, result map[string]any) ([]byte, error) {
	out := map[string]any{"tool": tool}
	for k, v := range result {
		out[k] = v
	}
	if c.d.Writer != nil {
		body, err := json.Marshal(map[string]any{
			"tool":       tool,
			"inputs":     json.RawMessage(domain.CanonicalJSON(orEmpty(call.ArgsJSON))),
			"input_refs": refs,
			"result":     result,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %v", tool, err)
		}
		ent, err := c.d.Writer.Record(ctx, domain.LedgerRecord{
			Kind:        domain.LedgerComputed,
			Provenance:  domain.LedgerProvenanceFromContext(ctx),
			ToolName:    tool,
			ArgsJSON:    orEmpty(call.ArgsJSON),
			Writer:      domain.AgentPrincipal(call.AgentID),
			DerivedFrom: refs,
			ContentType: "application/json",
			Body:        body,
		})
		if err != nil {
			// The calculation stands; only its record failed. Said, never hidden:
			// a figure from it cannot be cited.
			out["ledger_ref"] = ""
			out["note"] = "the result could not be recorded to the ledger (" + err.Error() + "); it cannot be cited as evidence"
		} else {
			out["ledger_ref"] = ent.ID
			out["note"] = "cite ledger_ref as the evidence of these figures"
		}
	}
	return json.Marshal(out)
}

func orEmpty(b []byte) []byte {
	if len(strings.TrimSpace(string(b))) == 0 {
		return []byte("{}")
	}
	return b
}

func decodeArgs(tool string, raw []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(orEmpty(raw))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: arguments: %v", tool, err)
	}
	return nil
}

// ParseDate reads a calendar date: ISO (YYYY-MM-DD or a timestamp) or
// day.month.year / day/month/year as Turkish and German records write it.
func ParseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, ok := domain.ParseDateValue(s); ok {
		return t, true
	}
	for _, sep := range []string{".", "/"} {
		parts := strings.Split(s, sep)
		if len(parts) != 3 {
			continue
		}
		d, e1 := strconv.Atoi(parts[0])
		m, e2 := strconv.Atoi(parts[1])
		y, e3 := strconv.Atoi(parts[2])
		if e1 != nil || e2 != nil || e3 != nil || m < 1 || m > 12 || d < 1 || d > 31 || y < 1000 {
			continue
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		if t.Day() != d { // 31.02.2026
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

func iso(t time.Time) string { return t.Format(time.DateOnly) }

// round cleans float noise from sums of money amounts (0.1+0.2), keeping six
// decimals: exact enough for any currency, stable to compare.
func round(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// numericString matches a string that IS a number or a money amount: digits
// with grouping and decimal marks, optionally a unit or currency after it
// ("1.234,50 TL", "80%"), or a currency code or symbol before it ("TRY 1,250",
// "€80"). An identifier such as "CUS-004" or a date is not a number, though
// domain.ParseAmount would find digits in it.
var numericString = regexp.MustCompile(`^\s*(?:[A-Za-z]{3}\s+|[€$£₺¥]\s*)?[-+]?\d[\d.,' \x{00A0}]*\s*(?:[A-Za-z]{1,3}|[€$£₺¥%])?\s*$`)

// number reads a numeric value: a JSON number, or a string that is one (money
// strings included: "1.234,50 TL", "€ 80").
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		if !numericString.MatchString(t) {
			return 0, false
		}
		return domain.ParseAmount(t)
	case bool, nil:
		return 0, false
	}
	return 0, false
}

// text is a value as text (a JSON string unquoted, anything else as JSON).
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
