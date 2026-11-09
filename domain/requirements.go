package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The requirement checklist and the answer envelope (ADR-0145 D5, D6;
// T-20261108-adr-0145-phase-2-requirement-checklist-with).
//
// A delegated request becomes a checklist of items derived from the user's own
// words (each item carries a verbatim quote, checked in code). Every plan step
// lists the items it answers and returns ONE fixed envelope: per item a status
// claim, typed values and the ledger entry each value came from. Whether an item is
// done is decided by code (internal/requirements), never by a model: the planner
// cannot set a status and a worker's status is a claim the kernel checks against
// the ledger.

// RequirementType is what an item asks for.
type RequirementType string

const (
	// RequirementAnswer is a question to answer.
	RequirementAnswer RequirementType = "answer"
	// RequirementFigure is a number, money amount or date to state.
	RequirementFigure RequirementType = "figure"
	// RequirementDocument is a draft or file to produce.
	RequirementDocument RequirementType = "document"
	// RequirementWrite is a change to make in a system.
	RequirementWrite RequirementType = "write"
	// RequirementConstraint is a limit the work must keep ("do not send yet").
	RequirementConstraint RequirementType = "constraint"
)

// ValidRequirementType reports whether t is one of the five item types.
func ValidRequirementType(t RequirementType) bool {
	switch t {
	case RequirementAnswer, RequirementFigure, RequirementDocument, RequirementWrite, RequirementConstraint:
		return true
	}
	return false
}

// RequirementStatus is an item's state. Only code changes it.
type RequirementStatus string

const (
	RequirementOpen         RequirementStatus = "open"
	RequirementSatisfied    RequirementStatus = "satisfied"
	RequirementNotFound     RequirementStatus = "not_found"
	RequirementNotPermitted RequirementStatus = "not_permitted"
	RequirementFailed       RequirementStatus = "failed"
)

// Final reports whether s ends the item (every status but open).
func (s RequirementStatus) Final() bool {
	switch s {
	case RequirementSatisfied, RequirementNotFound, RequirementNotPermitted, RequirementFailed:
		return true
	}
	return false
}

// ValidClaimStatus reports whether s is a status a worker may claim in an envelope.
func ValidClaimStatus(s RequirementStatus) bool { return s.Final() }

// RequirementItem is one checklist item.
type RequirementItem struct {
	// ID is "R1", "R2", ... assigned by code in derivation order.
	ID   string          `json:"id"`
	Type RequirementType `json:"type"`
	// Text says, in English, what the item requires.
	Text string `json:"text"`
	// Quote is the span of the user's message the item comes from, verbatim and
	// in the user's language (QuoteAnchored checks it).
	Quote string `json:"quote"`
	// Forbids names the effect terms a constraint rules out (send, write, ...),
	// from the effect vocabulary. Only on a constraint.
	Forbids []string `json:"forbids,omitempty"`
}

// RequirementChecklistVersion is the shape version of RequirementChecklist.
const RequirementChecklistVersion = 1

// RequirementChecklist is the derived checklist of one delegated request.
type RequirementChecklist struct {
	Version int `json:"version"`
	// Request is the user's own message the items were derived from.
	Request string            `json:"request"`
	Items   []RequirementItem `json:"items"`
	// Fallback is set when no derived item survived the quote check and the whole
	// request became the single item.
	Fallback bool `json:"fallback,omitempty"`
	// Dropped lists the items whose quote was not in the request after the one
	// repair (their text), for the trace.
	Dropped []string `json:"dropped,omitempty"`
	// Assumptions are what the staged planner assumed instead of asking the
	// person (ADR-0145 D5a.3: a path that cannot ask proceeds and states its
	// assumptions in the checklist, shown in the result).
	Assumptions []string `json:"assumptions,omitempty"`
	// LedgerRef is the checklist's own ledger entry, once recorded.
	LedgerRef string `json:"ledger_ref,omitempty"`
}

// Item returns the item with id, or nil.
func (c *RequirementChecklist) Item(id string) *RequirementItem {
	if c == nil {
		return nil
	}
	for i := range c.Items {
		if c.Items[i].ID == id {
			return &c.Items[i]
		}
	}
	return nil
}

// Constraints returns the constraint items.
func (c *RequirementChecklist) Constraints() []RequirementItem {
	if c == nil {
		return nil
	}
	var out []RequirementItem
	for _, it := range c.Items {
		if it.Type == RequirementConstraint {
			out = append(out, it)
		}
	}
	return out
}

// FallbackChecklist is the checklist of last resort: the whole request as one
// answer item, quoting all of it.
func FallbackChecklist(request string) RequirementChecklist {
	return RequirementChecklist{Version: RequirementChecklistVersion, Request: request, Fallback: true,
		Items: []RequirementItem{{ID: "R1", Type: RequirementAnswer, Text: "Answer the whole request.", Quote: strings.TrimSpace(request)}}}
}

type requirementChecklistKey struct{}

// WithRequirementChecklist carries the run's checklist on ctx for the planner and
// the plan validator.
func WithRequirementChecklist(ctx context.Context, c *RequirementChecklist) context.Context {
	return context.WithValue(ctx, requirementChecklistKey{}, c)
}

// RequirementChecklistFrom returns the checklist on ctx, or nil.
func RequirementChecklistFrom(ctx context.Context) *RequirementChecklist {
	c, _ := ctx.Value(requirementChecklistKey{}).(*RequirementChecklist)
	return c
}

var userRequestRe = regexp.MustCompile(`(?s)<user_request>\s*(.*?)\s*</user_request>`)

// UserRequestText is the user's own words in a delegated request: the text inside
// the last <user_request> region the chat agent appends to its delegation, or
// the whole request when there is none (an agent-to-agent Execute, a benchmark
// transport call, an ingress pipeline).
func UserRequestText(request string) string {
	m := userRequestRe.FindAllStringSubmatch(request, -1)
	if len(m) > 0 {
		if t := strings.TrimSpace(m[len(m)-1][1]); t != "" {
			return t
		}
	}
	return strings.TrimSpace(request)
}

// normalizeQuoteSpace collapses every run of whitespace to one space and folds
// typographic quotes and dashes, so a quote that differs from the message only in
// line breaks or in the quotation marks the model typed is still verbatim.
func normalizeQuoteSpace(s string) string {
	r := strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'", "–", "-", "—", "-")
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// QuoteAnchored reports whether quote appears verbatim in request (whitespace and
// typographic quotation marks normalised; case and every other character exact).
// An empty quote is never anchored.
func QuoteAnchored(request, quote string) bool {
	q := normalizeQuoteSpace(quote)
	if q == "" {
		return false
	}
	return strings.Contains(normalizeQuoteSpace(request), q)
}

// ─── The answer envelope (D6) ───────────────────────────────────────────────

// HandoffKeyAnswerItems carries, on a step's handoff, the checklist items the
// step answers (JSON []RequirementItem). Its presence is what tells the worker to
// finish with the answer envelope. Kernel-authored.
const HandoffKeyAnswerItems = "_answer_items"

// HandoffKeyEnvelopeViolations carries, on the one repair turn, what was wrong
// with the step's previous envelope (JSON []EnvelopeViolation). Kernel-authored.
const HandoffKeyEnvelopeViolations = "_envelope_violations"

// HandoffKeyRepairPriorCalls carries, on the one repair turn, the calls the step
// already made (JSON []RepairPriorCall): every tool result by ledger reference and
// every side-effecting call with its outcome, so the repair cites them instead of
// running them again (ADR-0145 D14; a write that executed is never re-run).
// Kernel-authored.
const HandoffKeyRepairPriorCalls = "_repair_prior_calls"

// RepairPriorCall is one call a step made before its repair turn.
type RepairPriorCall struct {
	Tool      string   `json:"tool"`
	LedgerRef string   `json:"ledger_ref,omitempty"`
	Preview   string   `json:"preview,omitempty"`
	Write     bool     `json:"write,omitempty"`
	Status    string   `json:"status,omitempty"`
	IDs       []string `json:"ids,omitempty"`
}

// AnswerEnvelopeVersion is the shape version of AnswerEnvelope.
const AnswerEnvelopeVersion = 1

// AnswerEnvelope is the one fixed answer shape every step returns when it answers
// checklist items (the SDK's final_answer, extended).
type AnswerEnvelope struct {
	AnswerText   string       `json:"answer_text"`
	ItemResults  []ItemResult `json:"item_results"`
	EvidenceRefs []string     `json:"evidence_refs,omitempty"`
}

// ItemResult is a worker's result for one item. Status is a CLAIM; code decides.
type ItemResult struct {
	Item   string            `json:"item"`
	Status RequirementStatus `json:"status"`
	Values []EnvelopeValue   `json:"values,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// EnvelopeValue is one typed value with the ledger entry it came from.
type EnvelopeValue struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Value       json.RawMessage `json:"value"`
	EvidenceRef string          `json:"evidence_ref"`
}

// Text is the value as text: a JSON string unquoted, anything else as JSON.
func (v EnvelopeValue) Text() string {
	var s string
	if json.Unmarshal(v.Value, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(v.Value))
}

// The value type vocabulary (D6).
const (
	ValueString   = "string"
	ValueNumber   = "number"
	ValueMoney    = "money"
	ValueDate     = "date"
	ValueID       = "id"
	ValueRef      = "ref"
	ValueCitation = "citation"
	ValueBool     = "bool"
	ValueList     = "list"
	ValueObject   = "object"
)

// ValueTypes lists the vocabulary, in the order the prompt shows it.
var ValueTypes = []string{ValueString, ValueNumber, ValueMoney, ValueDate, ValueID, ValueRef, ValueCitation, ValueBool, ValueList, ValueObject}

// FigureValueType reports whether t is a figure (D5: only a tool result or a
// calculation may back it, never model text).
func FigureValueType(t string) bool { return t == ValueNumber || t == ValueMoney || t == ValueDate }

// ParseAnswerEnvelope decodes a step output as an envelope. ok is false when the
// body is not a JSON object with item_results (a prose answer).
func ParseAnswerEnvelope(body []byte) (*AnswerEnvelope, bool) {
	t := strings.TrimSpace(string(body))
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		if nl := strings.IndexByte(t, '\n'); nl >= 0 {
			t = t[nl+1:]
		}
		t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
	}
	if !strings.HasPrefix(t, "{") {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &probe) != nil {
		return nil, false
	}
	if _, has := probe["item_results"]; !has {
		return nil, false
	}
	var env AnswerEnvelope
	if json.Unmarshal([]byte(t), &env) != nil {
		return nil, false
	}
	return &env, true
}

// CheckValueType reports why v.Value does not fit v.Type ("" when it fits).
func CheckValueType(v EnvelopeValue) string {
	raw := strings.TrimSpace(string(v.Value))
	if raw == "" || raw == "null" {
		return "the value is empty"
	}
	var any_ any
	if json.Unmarshal([]byte(raw), &any_) != nil {
		return "the value is not valid JSON"
	}
	str, isStr := any_.(string)
	switch v.Type {
	case ValueString, ValueCitation:
		if !isStr || strings.TrimSpace(str) == "" {
			return "a " + v.Type + " must be a non-empty string"
		}
	case ValueID:
		if !isStr || strings.TrimSpace(str) == "" {
			return "an id must be a non-empty string"
		}
	case ValueRef:
		if !isStr || !IsLedgerID(str) {
			return "a ref must be a ledger id (led_…)"
		}
	case ValueNumber, ValueMoney:
		if _, ok := any_.(float64); ok {
			return ""
		}
		if isStr {
			if _, ok := ParseAmount(str); ok {
				return ""
			}
		}
		return "a " + v.Type + " must be a number (or a string holding one)"
	case ValueDate:
		if !isStr {
			return "a date must be a string (YYYY-MM-DD)"
		}
		if _, ok := ParseDateValue(str); !ok {
			return "a date must be ISO 8601 (YYYY-MM-DD or a timestamp)"
		}
	case ValueBool:
		if _, ok := any_.(bool); !ok {
			return "a bool must be true or false"
		}
	case ValueList:
		if _, ok := any_.([]any); !ok {
			return "a list must be a JSON array"
		}
	case ValueObject:
		if _, ok := any_.(map[string]any); !ok {
			return "an object must be a JSON object"
		}
	default:
		return fmt.Sprintf("unknown value type %q (the types are %s)", v.Type, strings.Join(ValueTypes, ", "))
	}
	return ""
}

var amountRe = regexp.MustCompile(`[-+]?\d[\d.,' \x{00A0}]*`)

// ParseAmount reads the number in a money or number string ("1,234.50 EUR",
// "€ 1.234,50", "-80"). It understands "," and "." as either the decimal or the
// grouping mark by their position.
func ParseAmount(s string) (float64, bool) {
	m := amountRe.FindString(s)
	if m == "" {
		return 0, false
	}
	return parseNumberToken(m)
}

func parseNumberToken(m string) (float64, bool) {
	m = strings.TrimRight(strings.NewReplacer(" ", "", " ", "", "'", "").Replace(m), ".,")
	lastDot, lastComma := strings.LastIndex(m, "."), strings.LastIndex(m, ",")
	switch {
	case lastDot >= 0 && lastComma >= 0:
		if lastComma > lastDot { // 1.234,50
			m = strings.ReplaceAll(m, ".", "")
			m = strings.Replace(m, ",", ".", 1)
		} else { // 1,234.50
			m = strings.ReplaceAll(m, ",", "")
		}
	case lastComma >= 0:
		// One comma followed by exactly 3 digits is grouping (1,250); otherwise decimal.
		if strings.Count(m, ",") == 1 && len(m)-lastComma-1 != 3 {
			m = strings.Replace(m, ",", ".", 1)
		} else {
			m = strings.ReplaceAll(m, ",", "")
		}
	case lastDot >= 0:
		if strings.Count(m, ".") > 1 { // 1.234.567
			m = strings.ReplaceAll(m, ".", "")
		}
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ParseDateValue parses an ISO date or timestamp to its calendar date.
func ParseDateValue(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

var (
	isoDateRe    = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})`)
	dottedDateRe = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)
	bodyNumberRe = regexp.MustCompile(`[-+]?\d[\d.,]*`)
)

// EvidenceContains is the deep evidence check (D6): an id, number, money amount
// or date a value states must appear in the body of the entry it cites. Numbers
// compare by value (1250.5 matches "1,250.50"), dates by calendar day (ISO, or
// day.month.year as written in Turkish and German records), ids
// case-insensitively. Other types are not deep-checked (true).
func EvidenceContains(v EnvelopeValue, body []byte) bool {
	text := string(body)
	switch v.Type {
	case ValueID:
		id := strings.TrimSpace(v.Text())
		return id != "" && strings.Contains(strings.ToLower(text), strings.ToLower(id))
	case ValueNumber, ValueMoney:
		var want float64
		var ok bool
		var f float64
		if json.Unmarshal(v.Value, &f) == nil {
			want, ok = f, true
		} else {
			want, ok = ParseAmount(v.Text())
		}
		if !ok {
			return false
		}
		for _, tok := range bodyNumberRe.FindAllString(text, -1) {
			if got, ok := parseNumberToken(tok); ok && math.Abs(got-want) < 0.005 {
				return true
			}
			// "1250.5" is also written "1250,50" and vice versa; try the plain form.
			if got, err := strconv.ParseFloat(strings.TrimRight(tok, ".,"), 64); err == nil && math.Abs(got-want) < 0.005 {
				return true
			}
		}
		return false
	case ValueDate:
		want, ok := ParseDateValue(v.Text())
		if !ok {
			return false
		}
		for _, m := range isoDateRe.FindAllStringSubmatch(text, -1) {
			if d, ok := ParseDateValue(m[1] + "-" + m[2] + "-" + m[3]); ok && d.Equal(want) {
				return true
			}
		}
		for _, m := range dottedDateRe.FindAllStringSubmatch(text, -1) {
			day, _ := strconv.Atoi(m[1])
			mon, _ := strconv.Atoi(m[2])
			yr, _ := strconv.Atoi(m[3])
			if yr == want.Year() && time.Month(mon) == want.Month() && day == want.Day() {
				return true
			}
		}
		return false
	}
	return true
}

// EnvelopeViolation is one thing wrong with a step's envelope, structured so the
// repair turn can name it and the trace can count it.
type EnvelopeViolation struct {
	Item   string `json:"item,omitempty"`
	Value  string `json:"value,omitempty"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// Envelope violation rules.
const (
	ViolationNoEnvelope      = "no_envelope"
	ViolationUnknownItem     = "unknown_item"
	ViolationMissingItem     = "missing_item"
	ViolationBadStatus       = "bad_status"
	ViolationBadType         = "bad_type"
	ViolationNoEvidence      = "no_evidence"
	ViolationForeignEvidence = "foreign_evidence"
	ViolationFigureFromText  = "figure_from_model_text"
	ViolationNotInEvidence   = "not_in_evidence"
)

// StepInputValue is one resolved input binding handed to a step
// (HandoffKeyStepInputs).
type StepInputValue struct {
	Binding string          `json:"binding"`
	Step    int             `json:"step"`
	Ref     string          `json:"ref,omitempty"`
	Values  []EnvelopeValue `json:"values,omitempty"`
}

// ParseBinding splits "s3.overdue[*]" into (3, "overdue", true). ok is false for
// anything that is not s<digits>[.<name>][[*]].
func ParseBinding(b string) (step int, name string, list bool, ok bool) {
	b = strings.TrimSpace(b)
	if strings.HasSuffix(b, "[*]") {
		list = true
		b = strings.TrimSuffix(b, "[*]")
	}
	if len(b) < 2 || (b[0] != 's' && b[0] != 'S') {
		return 0, "", false, false
	}
	head, rest, _ := strings.Cut(b[1:], ".")
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return 0, "", false, false
	}
	for _, r := range rest {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return 0, "", false, false
		}
	}
	return n, rest, list, true
}

// CoverageStage is what a coverage stage's planner call is told (ADR-0145 D5.5):
// which stage this is, the items still open with why, and a compact view of the
// earlier stages' results (references, not data). Carried on ctx by the server.
type CoverageStage struct {
	Number int        `json:"stage"`
	Open   []OpenItem `json:"open"`
	// Settled lists the item ids already in a final state.
	Settled []string `json:"settled,omitempty"`
	// SettledAs is each settled item's final status (the staged planner shows it).
	SettledAs map[string]RequirementStatus `json:"settled_as,omitempty"`
	// Earlier is the rendered index of earlier stages' step results (ledger refs
	// and previews), shown in <documents>.
	Earlier string `json:"-"`
}

// OpenItem is one still-open item on a coverage stage.
type OpenItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
	// Missing are the for_each members with no result yet; the stage covers
	// exactly these.
	Missing []string `json:"missing,omitempty"`
	// Retry is set when the earlier attempt must not be repeated as it was (its
	// searches errored): try a different strategy or source.
	Retry string `json:"retry,omitempty"`
}

type coverageStageKey struct{}

// WithCoverageStage carries a coverage stage's notes on ctx for the planner.
func WithCoverageStage(ctx context.Context, s *CoverageStage) context.Context {
	return context.WithValue(ctx, coverageStageKey{}, s)
}

// CoverageStageFrom returns the coverage stage on ctx, or nil (the first stage).
func CoverageStageFrom(ctx context.Context) *CoverageStage {
	s, _ := ctx.Value(coverageStageKey{}).(*CoverageStage)
	return s
}
