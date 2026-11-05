package network

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/prompts"
)

// Thought-step synthesis (ADR-0139 C1/C4/D2).
//
// A thought step is the kernel answering a plan step itself from the results of the
// steps before it. Its request is two library prompts: thought_synthesis, the stable
// system block (cached), and thought_synthesis_input, the user message carrying the
// goal, the task and the results as tagged data. The results are ordered
// deterministically (step number for the legacy context map, plan order for the
// working-memory refs) and bounded by execution.plan.thought_results_max_chars, so the
// same plan state always sends the same bytes and a large content-store body cannot
// blow the context window.

const (
	thoughtSystemPromptID = "thought_synthesis"
	thoughtInputPromptID  = "thought_synthesis_input"
)

// thoughtDataTags are the data regions of thought_synthesis_input.
var thoughtDataTags = []string{"goal", "task", "results", "result"}

// thoughtResult is one earlier-step result to inline.
type thoughtResult struct {
	source string // the context key or the content id it came from
	text   string
}

// thoughtGenerator is the generator a thought step is served by: ThoughtLLM (the
// planner-purpose generator, a C1 RequestGenerator) when wired, else the Planner's
// legacy Generate, which domain.GenerateVia serves as one flattened prompt.
func (s *Server) thoughtGenerator() domain.Generator {
	if s.ThoughtLLM != nil {
		return s.ThoughtLLM
	}
	return s.Planner
}

// thoughtResults collects the results a thought step synthesises, in a deterministic
// order: the legacy step_N_result context keys by step number (the map's own order
// is random), then the working-memory refs in the order the plan produced them.
func (s *Server) thoughtResults(ctx context.Context, handoff *domain.Handoff) []thoughtResult {
	var out []thoughtResult
	keys := make([]string, 0, len(handoff.Context))
	for k := range handoff.Context {
		if strings.HasPrefix(k, "step_") && strings.HasSuffix(k, "_result") {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		ni, ei := stepKeyNumber(keys[i])
		nj, ej := stepKeyNumber(keys[j])
		if ei == nil && ej == nil && ni != nj {
			return ni < nj
		}
		if (ei == nil) != (ej == nil) {
			return ei == nil // numbered keys first
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		out = append(out, thoughtResult{source: k, text: handoff.Context[k]})
	}
	// ADR-0022 Phase 3: when UseGlobalWorkspace=true, Context is nil and prior step
	// outputs are carried as ContextRefs in WorkingMemory.
	for _, ref := range handoff.WorkingMemory {
		if ref.Snippet != "" {
			out = append(out, thoughtResult{source: string(ref.CID), text: ref.Snippet})
			continue
		}
		if s.ContentStore == nil {
			continue
		}
		if node, err := s.ContentStore.Get(ctx, ref.CID); err == nil && node != nil {
			out = append(out, thoughtResult{source: string(ref.CID), text: string(node.Data)})
		}
	}
	return out
}

// stepKeyNumber reads N from "step_N_result".
func stepKeyNumber(k string) (int, error) {
	return strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(k, "step_"), "_result"))
}

// fitThoughtResults shortens results so their texts total at most budget characters
// (bytes, cut on a rune boundary). Every result keeps a fair share: the budget is
// split evenly, a result smaller than its share gives the rest back to the others,
// and only results larger than the final share are cut (and marked). budget <= 0 is
// the config default.
func fitThoughtResults(results []thoughtResult, budget int) []thoughtResult {
	if budget <= 0 {
		budget = config.DefaultThoughtResultsMaxChars
	}
	total := 0
	for _, r := range results {
		total += len(r.text)
	}
	if total <= budget || len(results) == 0 {
		return results
	}
	// Water-filling: find the share every over-sized result is cut to.
	sizes := make([]int, len(results))
	for i, r := range results {
		sizes[i] = len(r.text)
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
	out := make([]thoughtResult, len(results))
	for i, r := range results {
		out[i] = r
		if len(r.text) > share {
			cut := share
			for cut > 0 && !utf8.RuneStart(r.text[cut]) {
				cut--
			}
			out[i].text = r.text[:cut] + fmt.Sprintf("\n[shortened: %d of %d characters shown]", cut, len(r.text))
		}
	}
	return out
}

// buildThoughtRequest renders one thought step's synthesis call.
func buildThoughtRequest(goal, task string, results []thoughtResult) (domain.GenerateRequest, error) {
	sys, err := prompts.Get(thoughtSystemPromptID)
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	in, err := prompts.Get(thoughtInputPromptID)
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	var rb strings.Builder
	for i, r := range results {
		if i > 0 {
			rb.WriteByte('\n')
		}
		fmt.Fprintf(&rb, "<result source=%q>\n%s\n</result>", r.source,
			prompts.NeutraliseClosingTags(r.text, thoughtDataTags...))
	}
	msg, err := in.Render(map[string]string{
		"goal":    prompts.NeutraliseClosingTags(strings.TrimSpace(goal), thoughtDataTags...),
		"task":    prompts.NeutraliseClosingTags(strings.TrimSpace(task), thoughtDataTags...),
		"results": rb.String(),
	})
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	return domain.GenerateRequest{
		System:   []domain.SystemBlock{{Text: sys.Body, Cache: true}},
		Messages: []domain.ModelMessage{domain.UserMessage(msg.Text)},
	}, nil
}
