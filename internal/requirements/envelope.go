// Package requirements is the kernel's code-decided coverage of a delegated
// request (ADR-0145 D5, D6; T-20261108-adr-0145-phase-2-requirement-checklist-with):
// the checklist derivation (Deriver), the answer-envelope validation every step
// output passes (ValidateEnvelope), and the item state machine that decides, from
// the ledger and never from a model, when an item is satisfied, not found, not
// permitted or failed, and when the work has stopped making progress (Coverage).
package requirements

import (
	"fmt"
	"strings"

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
			if why := checkValue(v, ev); why != nil {
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
		if _, ok := ev.Entries[ref]; !ok {
			vs = append(vs, domain.EnvelopeViolation{Rule: domain.ViolationForeignEvidence,
				Detail: fmt.Sprintf("evidence_refs names %q, which is not a result of this step's own tool calls or inputs", ref)})
		}
	}
	return out, vs
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
			Detail: fmt.Sprintf("%s is not a result of this step's own tool calls or inputs", v.EvidenceRef)}
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
