// Package requirements is the kernel's code-decided coverage of a delegated
// request (ADR-0145 D5, D6; T-20261108-adr-0145-phase-2-requirement-checklist-with):
// the checklist derivation (Deriver), the answer-envelope validation every step
// output passes (ValidateEnvelope), and the item state machine that decides, from
// the ledger and never from a model, when an item is satisfied, not found, not
// permitted or failed, and when the work has stopped making progress (Coverage).
package requirements

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// Evidence is what a step may cite (D6): the ledger entries of its own tool calls
// and calculations, and of its inputs (the upstream step outputs it was handed and
// the entries their validated envelopes cited). Entries carry their bodies for the
// deep check. Built by the DAG executor from the run ledger.
type Evidence struct {
	Entries map[string]domain.LedgerEntry
}

// ValidateEnvelope checks a step's envelope against the items the step answers and
// the evidence it may cite. It returns the results with every invalid value
// removed (what code accepts) and the violations (what the repair turn names).
//
// Rules: every item of the step has a result and no result names another item; a
// claimed status is a final status; every value has a known type and fits it, and
// cites an entry the step may cite; a figure (number, money, date) cites a tool
// result or a calculation, never a step output (model text); and an id, number,
// money amount or date appears in the entry it cites (the deep check). An entry
// stored truncated is not deep-checked past what was stored: the value is kept,
// since absence from a cut body proves nothing.
func ValidateEnvelope(env *domain.AnswerEnvelope, items []domain.RequirementItem, ev Evidence) ([]domain.ItemResult, []domain.EnvelopeViolation) {
	if env == nil {
		return nil, []domain.EnvelopeViolation{{Rule: domain.ViolationNoEnvelope,
			Detail: "the answer is not the envelope: finish with answer_text and one item_results entry per item"}}
	}
	want := map[string]bool{}
	for _, it := range items {
		want[it.ID] = true
	}
	var out []domain.ItemResult
	var vs []domain.EnvelopeViolation
	seen := map[string]bool{}
	for _, r := range env.ItemResults {
		if !want[r.Item] {
			vs = append(vs, domain.EnvelopeViolation{Item: r.Item, Rule: domain.ViolationUnknownItem,
				Detail: fmt.Sprintf("item %q is not one this step answers (%s)", r.Item, joinIDs(items))})
			continue
		}
		seen[r.Item] = true
		if !domain.ValidClaimStatus(r.Status) {
			vs = append(vs, domain.EnvelopeViolation{Item: r.Item, Rule: domain.ViolationBadStatus,
				Detail: fmt.Sprintf("status %q: use satisfied, not_found, not_permitted or failed", r.Status)})
		}
		kept := r
		kept.Values = nil
		for _, v := range r.Values {
			v, why := resolveRecordIDs(v, ev)
			if why == nil {
				why = checkValue(v, ev)
			}
			if why != nil {
				why.Item = r.Item
				vs = append(vs, *why)
				continue
			}
			kept.Values = append(kept.Values, v)
		}
		out = append(out, kept)
	}
	for _, it := range items {
		if it.Type != domain.RequirementConstraint && !seen[it.ID] {
			vs = append(vs, domain.EnvelopeViolation{Item: it.ID, Rule: domain.ViolationMissingItem,
				Detail: fmt.Sprintf("no item_results entry for %s (%s)", it.ID, it.Text)})
		}
	}
	for _, ref := range env.EvidenceRefs {
		if _, ok := ev.Entries[ref]; ok {
			continue
		}
		if !domain.IsLedgerID(ref) {
			if _, n := ev.entryContaining(ref); n == 1 {
				continue // a record id the step's evidence holds (resolved like a value's)
			}
		}
		vs = append(vs, domain.EnvelopeViolation{Rule: domain.ViolationForeignEvidence,
			Detail: fmt.Sprintf("evidence_refs names %q, which is not a result of this step's own tool calls or inputs; %s", ref, ev.available())})
	}
	return out, vs
}

// resolveRecordIDs is the deterministic repair of a value that cites or holds a
// record identifier of a system where a ledger id belongs
// (T-20261109-d6-envelope-keeps-items-open-when-the-model: "Draft MSG-000173" typed
// ref, a case reference as evidence_ref). No model is involved:
//   - an evidence_ref that is not a ledger id is replaced by the ONE entry of the
//     step's evidence whose content contains it;
//   - a ref value that is not a ledger id is the record identifier it names: the
//     value becomes an id (the identifier as the evidence states it) citing the ONE
//     entry whose content contains it (the cited entry when it already does).
//
// Zero or several matching entries keep the item open with a violation that lists
// the ledger ids the step may cite.
func resolveRecordIDs(v domain.EnvelopeValue, ev Evidence) (domain.EnvelopeValue, *domain.EnvelopeViolation) {
	if ref := strings.TrimSpace(v.EvidenceRef); ref != "" && !domain.IsLedgerID(ref) {
		id, n := ev.entryContaining(ref)
		if n != 1 {
			return v, &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationForeignEvidence,
				Detail: fmt.Sprintf("evidence_ref %q is not a ledger id and %s; %s", ref, matchCount(n), ev.available())}
		}
		v.EvidenceRef = id
	}
	if v.Type != domain.ValueRef {
		return v, nil
	}
	text := strings.TrimSpace(v.Text())
	if text == "" || domain.IsLedgerID(text) {
		return v, nil
	}
	// The cited entry holds the identifier: it is the evidence.
	if e, ok := ev.Entries[v.EvidenceRef]; ok {
		if needle := recordNeedle(text, func(n string) bool { return bodyContains(e.Body, n) }); needle != "" {
			return asID(v, needle, v.EvidenceRef), nil
		}
	}
	for _, needle := range needleCandidates(text) {
		id, n := ev.entryContaining(needle)
		if n == 1 {
			return asID(v, needle, id), nil
		}
		if n > 1 {
			return v, &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationBadType,
				Detail: fmt.Sprintf("a ref must be a ledger id (led_…); %q appears in %d entries, so it names no single one: cite the entry as evidence_ref and give the identifier as type id; %s", needle, n, ev.available())}
		}
	}
	return v, &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationBadType,
		Detail: fmt.Sprintf("a ref must be a ledger id (led_…); %q is not one and no entry of this step contains it: give a record identifier as type id citing the entry it appears in; %s", text, ev.available())}
}

func asID(v domain.EnvelopeValue, needle, entry string) domain.EnvelopeValue {
	raw, _ := json.Marshal(needle)
	v.Type, v.Value, v.EvidenceRef = domain.ValueID, raw, entry
	return v
}

func matchCount(n int) string {
	if n == 0 {
		return "no entry of this step contains it"
	}
	return fmt.Sprintf("%d entries of this step contain it, so it names no single one", n)
}

// needleCandidates are what a free-text ref may name, most specific first: the
// whole text, then each token that carries a digit (an identifier such as
// MSG-000173 or FOI-2026-0001 inside "Draft MSG-000173").
func needleCandidates(text string) []string {
	out := []string{text}
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;:()[]{}\"'`", r)
	}) {
		tok = strings.Trim(tok, ".")
		if len(tok) >= 3 && strings.IndexFunc(tok, unicode.IsDigit) >= 0 && tok != text {
			out = append(out, tok)
		}
	}
	return out
}

// recordNeedle is the first candidate of text that has(candidate).
func recordNeedle(text string, has func(string) bool) string {
	for _, n := range needleCandidates(text) {
		if has(n) {
			return n
		}
	}
	return ""
}

func bodyContains(body []byte, needle string) bool {
	return needle != "" && strings.Contains(strings.ToLower(string(body)), strings.ToLower(needle))
}

// entryContaining returns the one evidence entry whose content contains needle
// (case-insensitively) and how many do.
func (ev Evidence) entryContaining(needle string) (string, int) {
	needle = strings.TrimSpace(needle)
	if len(needle) < 3 {
		return "", 0
	}
	found, n := "", 0
	for _, id := range ev.ids() {
		if bodyContains(ev.Entries[id].Body, needle) {
			found, n = id, n+1
		}
	}
	if n != 1 {
		return "", n
	}
	return found, 1
}

func (ev Evidence) ids() []string {
	ids := make([]string, 0, len(ev.Entries))
	for id := range ev.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// maxListedEvidence bounds the ledger ids a violation lists.
const maxListedEvidence = 20

// available names the ledger ids the step may cite, for a violation message.
func (ev Evidence) available() string {
	ids := ev.ids()
	if len(ids) == 0 {
		return "this step has no ledger entries to cite: call a tool for the evidence"
	}
	parts := make([]string, 0, len(ids))
	for i, id := range ids {
		if i == maxListedEvidence {
			parts = append(parts, fmt.Sprintf("… %d more", len(ids)-maxListedEvidence))
			break
		}
		e := ev.Entries[id]
		label := string(e.Kind)
		if e.ToolName != "" {
			label = e.ToolName
		}
		parts = append(parts, id+" ("+label+")")
	}
	return "the ledger ids this step may cite: " + strings.Join(parts, ", ")
}

func checkValue(v domain.EnvelopeValue, ev Evidence) *domain.EnvelopeViolation {
	if why := domain.CheckValueType(v); why != "" {
		return &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationBadType, Detail: why}
	}
	if strings.TrimSpace(v.EvidenceRef) == "" {
		return &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationNoEvidence,
			Detail: "every value cites the ledger entry (led_…) it was read from in evidence_ref"}
	}
	e, ok := ev.Entries[v.EvidenceRef]
	if !ok {
		return &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationForeignEvidence,
			Detail: fmt.Sprintf("%s is not a result of this step's own tool calls or inputs; %s", v.EvidenceRef, ev.available())}
	}
	if domain.FigureValueType(v.Type) && e.Kind != domain.LedgerToolResult && e.Kind != domain.LedgerComputed {
		return &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationFigureFromText,
			Detail: fmt.Sprintf("a %s must cite a tool result or a calculation, not %s (%s)", v.Type, e.Kind, v.EvidenceRef)}
	}
	if !e.Truncated && !domain.EvidenceContains(v, e.Body) {
		return &domain.EnvelopeViolation{Value: v.Name, Rule: domain.ViolationNotInEvidence,
			Detail: fmt.Sprintf("%s %q does not appear in %s", v.Type, v.Text(), v.EvidenceRef)}
	}
	return nil
}

func joinIDs(items []domain.RequirementItem) string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return strings.Join(ids, ", ")
}
