package awareness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Effect-checked dispatch, planner side (T-20261106-kernel-does-not-check-that-a-declared,
// owner option B). Under the capability contract arm the planner (planner_cap v3) and
// the replanner (replan_cap v3) declare each action step's required_effects. This file
// renders what they are shown — the configured vocabulary and each agent's
// tool-derived effects, as per-call DATA in <capability_clusters> (never static text:
// the vocabulary is config and the agents' effects change live) — constrains the reply
// schema to the vocabulary, and checks what came back.

// EffectContractSource is what the replan handler needs from the planner to keep the
// effect contract on a replan: the rendered effect block and the vocabulary. *Planner
// implements it.
type EffectContractSource interface {
	EffectContract(ctx context.Context) (block string, vocab *domain.StepEffectVocabulary)
}

// SetEffectSource wires the effect source (the vocabulary plus each agent's derived
// effects). nil ⇒ no effect block and no schema enum; a step's declared effects then
// still reach dispatch, which refuses any it cannot verify.
func (p *Planner) SetEffectSource(src domain.AgentEffectSource) { p.effects = src }

// EffectContract renders the effect block for the current agents.
func (p *Planner) EffectContract(ctx context.Context) (string, *domain.StepEffectVocabulary) {
	if p.effects == nil {
		return "", nil
	}
	agents, err := p.provider.GetAllAgents(ctx)
	if err != nil {
		slog.WarnContext(ctx, "planner: agent list unavailable for the effect block", "err", err)
		agents = nil
	}
	return buildEffectBlock(ctx, p.effects, agents), p.effects.Vocabulary()
}

// buildEffectBlock renders the vocabulary and, per routable agent, the effects its
// callable tools produce. An agent whose tools produce none is listed as "(none)" so
// the planner can see it cannot act, not merely that it was left out.
func buildEffectBlock(ctx context.Context, src domain.AgentEffectSource, agents []domain.AgentDefinition) string {
	if src == nil {
		return ""
	}
	var sb strings.Builder
	vocab := src.Vocabulary()
	sb.WriteString("Effects (the required_effects vocabulary):\n")
	terms := vocab.Terms()
	if len(terms) == 0 {
		sb.WriteString("- (no effect vocabulary is configured; no step can declare an effect)\n")
	}
	for _, t := range terms {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
	}
	var lines []string
	for _, a := range agents {
		if a.Trait == domain.TraitModel || a.Trait == domain.TraitDaemon || domain.IsSystemDefinition(a) {
			continue // never dispatched a plan step
		}
		effs := src.AgentEffects(ctx, a.ID)
		list := "(none)"
		if len(effs) > 0 {
			list = strings.Join(effs, ", ")
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", a.ID, list))
	}
	if len(lines) > 0 {
		sort.Strings(lines)
		sb.WriteString("\nAgent effects (what each agent's tools can do):\n")
		sb.WriteString(strings.Join(lines, "\n"))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// appendBlock joins the capability block and the effect block.
func appendBlock(capBlock, effectBlock string) string {
	switch {
	case effectBlock == "":
		return capBlock
	case capBlock == "":
		return effectBlock
	default:
		return capBlock + "\n\n" + effectBlock
	}
}

// withEffectEnum returns the plan schema with steps[].required_effects.items
// constrained to the vocabulary, so the provider layer validates (and once repairs)
// a reply naming an effect outside it. The static schema file cannot carry the enum:
// the vocabulary is config. A schema without the property is returned unchanged.
// An empty vocabulary becomes maxItems 0: no effect can be declared.
func withEffectEnum(schema []byte, vocab *domain.StepEffectVocabulary) ([]byte, error) {
	if len(schema) == 0 || vocab == nil {
		return schema, nil
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("plan schema: %w", err)
	}
	props, _ := root["properties"].(map[string]any)
	steps, _ := props["steps"].(map[string]any)
	items, _ := steps["items"].(map[string]any)
	stepProps, _ := items["properties"].(map[string]any)
	re, ok := stepProps["required_effects"].(map[string]any)
	if !ok {
		return schema, nil
	}
	names := vocab.Names()
	if len(names) == 0 {
		re["maxItems"] = 0
	} else {
		it, _ := re["items"].(map[string]any)
		if it == nil {
			it = map[string]any{"type": "string"}
		}
		enum := make([]any, len(names))
		for i, n := range names {
			enum[i] = n
		}
		it["enum"] = enum
		re["items"] = it
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<capability_clusters>" readable in the schema text
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("plan schema: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// applyEffectSchema swaps the request's response schema for the enum-constrained one.
func applyEffectSchema(req *domain.GenerateRequest, vocab *domain.StepEffectVocabulary) error {
	if req.ResponseSchema == nil || vocab == nil {
		return nil
	}
	s, err := withEffectEnum(req.ResponseSchema.Schema, vocab)
	if err != nil {
		return err
	}
	req.ResponseSchema.Schema = s
	return nil
}

// normalizeDeclaredEffects lowercases each step's declared effects and logs the ones
// outside the vocabulary. Unknown effects are KEPT, never dropped: unlike an invented
// capability tag (dropInventedCapabilities, a routing refinement that fails open),
// an effect is a fail-closed requirement, and dropping one would quietly turn "needs
// an effect nobody has" into "needs nothing". Dispatch treats it as unmatched.
func normalizeDeclaredEffects(ctx context.Context, plan *domain.ExecutionPlan, vocab *domain.StepEffectVocabulary) {
	for i := range plan.Steps {
		effs := plan.Steps[i].RequiredEffects
		if effs == nil {
			continue
		}
		out := make([]string, 0, len(effs))
		seen := map[string]bool{}
		for _, e := range effs {
			n := domain.NormalizeStepEffect(e)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if vocab != nil && !vocab.Known(n) {
				slog.WarnContext(ctx, "planner: step declares an effect outside the vocabulary; dispatch will refuse it",
					"step", i, "effect", n, "vocabulary", vocab.Names())
			}
			out = append(out, n)
		}
		plan.Steps[i].RequiredEffects = out
	}
}
