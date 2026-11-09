package requirements

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// checklistPrompts is the derivation's library pair: prompts/requirement_checklist
// (the cached system block, with checklist.schema.json) and
// prompts/requirement_checklist_input (the effect vocabulary, the repair notes and,
// last, the request, as tagged data).
var checklistPrompts = prompts.MustPair("requirement_checklist", "requirement_checklist_input")

var checklistDataTags = []string{"effects", "repair", "request"}

func init() {
	h := checklistPrompts.StaticHash()
	domain.PromptRegistry[h] = domain.PromptEntry{
		ID: "requirements.checklist",
		Version: fmt.Sprintf("%s.v%d+%s.v%d", checklistPrompts.System.ID, checklistPrompts.System.Version,
			checklistPrompts.Input.ID, checklistPrompts.Input.Version),
		Hash:   h,
		Schema: string(checklistPrompts.Schema()),
	}
}

// Deriver builds a request's requirement checklist (ADR-0145 D5 "Derivation"):
// ONE strict-schema model call over the user's own message — never the chat
// agent's paraphrase — whose every item carries a verbatim quote that code checks.
// An unanchored item gets one repair call; an item still unanchored is dropped;
// when nothing survives the checklist is the whole request as one item. The
// proxy in front of the model does not enforce the response schema, so the reply
// is decoded and every field checked here as well (the provider layer's schema
// validation is a second line, never relied on).
type Deriver struct {
	LLM domain.Generator
	// Constraints are the terms a constraint's forbids are drawn from, each with
	// the ADR-0086 tool classes it rules out (execution.tools.constraint_classes;
	// ADR-0145 D14: constraints hold by tool class). nil shows none and forbids
	// are dropped.
	Constraints map[string][]domain.ToolEffect
}

type modelItem struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	Quote   string   `json:"quote"`
	Forbids []string `json:"forbids"`
	Does    []string `json:"does"`
}

type modelChecklist struct {
	Items []modelItem `json:"items"`
}

// Derive returns the checklist of request (the user's own words; see
// domain.UserRequestText). It never fails the request: a model error or an
// undecodable reply yields the fallback checklist, logged.
func (d *Deriver) Derive(ctx context.Context, request string) domain.RequirementChecklist {
	request = strings.TrimSpace(request)
	if d == nil || d.LLM == nil || request == "" {
		return domain.FallbackChecklist(request)
	}
	items, err := d.call(ctx, request, "")
	if err != nil {
		slog.Warn("ADR-0145 D5: checklist derivation failed; the whole request is the one item", "err", err)
		return domain.FallbackChecklist(request)
	}
	good, bad := d.anchor(request, items)
	var dropped []string
	if len(bad) > 0 {
		var notes []string
		for _, b := range bad {
			notes = append(notes, fmt.Sprintf("- %q (item: %s)", b.Quote, b.Text))
		}
		repaired, rerr := d.call(ctx, request, strings.Join(notes, "\n"))
		if rerr != nil {
			slog.Warn("ADR-0145 D5: checklist repair call failed; unanchored items dropped", "err", rerr)
			for _, b := range bad {
				dropped = append(dropped, b.Text)
			}
		} else {
			good, bad = d.anchor(request, repaired)
			for _, b := range bad {
				dropped = append(dropped, b.Text)
			}
		}
	}
	if len(dropped) > 0 {
		slog.Info("ADR-0145 D5: checklist items dropped (quote not in the request after one repair)", "dropped", dropped)
	}
	if len(good) == 0 {
		cl := domain.FallbackChecklist(request)
		cl.Dropped = dropped
		return cl
	}
	cl := domain.RequirementChecklist{Version: domain.RequirementChecklistVersion, Request: request, Dropped: dropped}
	for i, it := range good {
		it.ID = fmt.Sprintf("R%d", i+1)
		cl.Items = append(cl.Items, it)
	}
	return cl
}

// anchor splits decoded items into the ones code accepts (known type, non-empty
// text, quote verbatim in the request) and the ones that need repair (an
// unanchored quote). Items with an unknown type or no text are dropped outright:
// a repair cannot fix a shape the schema forbids.
func (d *Deriver) anchor(request string, items []modelItem) (good []domain.RequirementItem, bad []modelItem) {
	for _, m := range items {
		t := domain.RequirementType(strings.ToLower(strings.TrimSpace(m.Type)))
		if !domain.ValidRequirementType(t) || strings.TrimSpace(m.Text) == "" {
			continue
		}
		if !domain.QuoteAnchored(request, m.Quote) {
			bad = append(bad, m)
			continue
		}
		it := domain.RequirementItem{Type: t, Text: strings.TrimSpace(m.Text), Quote: strings.TrimSpace(m.Quote)}
		if t == domain.RequirementConstraint {
			it.Forbids = d.vocabulary(m.Forbids)
		}
		if t == domain.RequirementWrite {
			// ADR-0145 D14.6: the effects the write itself is; a constraint never
			// refuses them on the step that carries the write out.
			it.Does = d.vocabulary(m.Does)
		}
		good = append(good, it)
	}
	return good, bad
}

// vocabulary keeps the terms of the constraint vocabulary (normalised, unique,
// in order); anything else is dropped.
func (d *Deriver) vocabulary(terms []string) []string {
	var out []string
	for _, f := range terms {
		n := domain.NormalizeStepEffect(f)
		if _, known := d.Constraints[n]; known && !containsStr(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func (d *Deriver) call(ctx context.Context, request, repair string) ([]modelItem, error) {
	values := map[string]string{"effects": d.effectsBlock(), "repair": repair, "request": request}
	for k, v := range values {
		values[k] = prompts.NeutraliseClosingTags(strings.TrimRight(v, "\n"), checklistDataTags...)
	}
	msg, err := checklistPrompts.Message(values)
	if err != nil {
		return nil, fmt.Errorf("checklist prompt: %w", err)
	}
	resp, err := domain.GenerateVia(ctx, d.LLM, domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: checklistPrompts.System.Body, Cache: true}},
		Messages:       []domain.ModelMessage{domain.UserMessage(msg)},
		ResponseSchema: &domain.ResponseSchema{Name: checklistPrompts.System.ID, Schema: checklistPrompts.Schema()},
		CacheTTL:       domain.CacheTTLLong,
	})
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(resp.Text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	var m modelChecklist
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, fmt.Errorf("checklist reply is not the schema's JSON: %w", err)
	}
	return m.Items, nil
}

// classMeaning is what each ADR-0086 tool class covers, in the words the
// deriver's model reads.
var classMeaning = map[domain.ToolEffect]string{
	domain.EffectRead:   "reading or looking up anything",
	domain.EffectWrite:  "changing data in a system (create, update, draft, delete, move)",
	domain.EffectEgress: "anything that leaves the organisation (sending mail or messages, posting, publishing)",
	domain.EffectSpend:  "spending money (payments, purchases)",
	domain.EffectAdmin:  "changing permissions or configuration",
}

// effectsBlock lists the constraint terms (sorted) with what each rules out.
func (d *Deriver) effectsBlock() string {
	if len(d.Constraints) == 0 {
		return ""
	}
	terms := make([]string, 0, len(d.Constraints))
	for t := range d.Constraints {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	var lines []string
	for _, t := range terms {
		var parts []string
		for _, c := range d.Constraints[t] {
			parts = append(parts, classMeaning[c])
		}
		lines = append(lines, "- "+t+": rules out "+strings.Join(parts, "; "))
	}
	return strings.Join(lines, "\n")
}
