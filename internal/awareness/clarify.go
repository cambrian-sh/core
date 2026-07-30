package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Clarification is the operator-facing interview: turning a vague goal into an
// answerable one BEFORE a plan is built.
//
// Deliberately NOT in internal/metabolism/interview. That package is the AGENT
// examiner — it interviews agents to establish their declared capabilities
// (ROUTE-03). One word, two subsystems: this one interviews the human. Putting
// them together would be the kind of collision that reads as reuse and is not.
//
// It exists because planner output is the ceiling on routing quality
// (ADR-0100 D3). A vague goal cannot be recovered downstream — every later stage
// is working from the plan — so asking is cheaper than planning badly.

// ClarificationQuestion is one thing the operator must decide.
type ClarificationQuestion struct {
	Question string
	// Kind is KindScope or KindJudgement. They read differently and render
	// differently: a scope question has countable options, a judgement one does
	// not.
	Kind    string
	Options []ClarificationOption
	// WhyItChangesTheAnswer states what actually differs between the options. A
	// question without it reads as bureaucracy and gets answered at random.
	WhyItChangesTheAnswer string
}

// Question kinds.
const (
	KindScope     = "scope"
	KindJudgement = "judgement"
)

// ClarificationOption is one answer.
type ClarificationOption struct {
	Label string
	// Tag is the classification tag this option narrows to, when it has one. It
	// is what makes DocumentCount computable.
	Tag string
	// DocumentCount is how many documents the option reaches, or -1 when it
	// cannot be counted. -1 is NOT zero: a zero-document option is a real and
	// alarming answer ("this would search nothing"), and "we could not count" is
	// a different statement entirely.
	DocumentCount int
	Detail        string
}

// DocumentCounter counts documents carrying a classification tag, resolved
// against the CALLER's reachable set. nil ⇒ counts report -1.
type DocumentCounter interface {
	CountByTag(ctx context.Context, tag string) int
}

// Clarifier decides whether a goal is answerable and, when it is not, what to
// ask.
type Clarifier struct {
	LLM domain.Generator
	// Vocabulary is the real classification vocabulary. Options are drawn from it
	// and never coined: a hallucinated tag produces a scope that matches nothing
	// and looks exactly like a working choice.
	Vocabulary []string
	Documents  DocumentCounter
}

// modelClarification is the JSON contract with the model. Flat and small, with
// nowhere to smuggle an instruction.
type modelClarification struct {
	// Answerable false ⇒ the goal needs at least one question resolved.
	Answerable bool   `json:"answerable"`
	Reason     string `json:"reason"`
	Questions  []struct {
		Question              string `json:"question"`
		Kind                  string `json:"kind"`
		WhyItChangesTheAnswer string `json:"why_it_changes_the_answer"`
		Options               []struct {
			Label string `json:"label"`
			Tag   string `json:"tag"`
		} `json:"options"`
	} `json:"questions"`
}

// Clarify returns the questions a goal needs answered, or nil when it is
// answerable as written.
//
// A nil LLM returns no questions rather than an error: an absent clarifier is a
// missing convenience, never a missing check, and refusing to plan without one
// would make the assistant a hard dependency of the whole propose path.
func (c *Clarifier) Clarify(ctx context.Context, goal string, answers []string) ([]ClarificationQuestion, error) {
	if c == nil || c.LLM == nil {
		return nil, nil
	}
	if strings.TrimSpace(goal) == "" {
		return nil, fmt.Errorf("awareness: a clarification needs a goal")
	}
	// Answers already given mean this round has been through the interview. One
	// round only: a second would be an interrogation, and the operator's move
	// after answering is to see a plan.
	if len(answers) > 0 {
		return nil, nil
	}

	raw, err := c.LLM.Generate(ctx, c.buildPrompt(goal))
	if err != nil {
		// Fail OPEN. The clarifier's failure must not block proposing: the
		// operator still gets a plan to inspect, which is strictly better than an
		// error page.
		return nil, nil
	}

	var m modelClarification
	if uerr := json.Unmarshal([]byte(domain.ExtractJSONObject(raw)), &m); uerr != nil {
		return nil, nil
	}
	if m.Answerable {
		return nil, nil
	}

	var out []ClarificationQuestion
	for _, q := range m.Questions {
		text := strings.TrimSpace(q.Question)
		if text == "" {
			continue
		}
		cq := ClarificationQuestion{
			Question:              text,
			Kind:                  normaliseKind(q.Kind),
			WhyItChangesTheAnswer: strings.TrimSpace(q.WhyItChangesTheAnswer),
		}
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				continue
			}
			opt := ClarificationOption{Label: label, Tag: strings.TrimSpace(o.Tag), DocumentCount: -1}
			// Only a tag the vocabulary actually holds gets counted. A coined tag
			// would otherwise report 0 documents and read as "this option is
			// empty" rather than "this option is not real".
			if opt.Tag != "" && c.knowsTag(opt.Tag) && c.Documents != nil {
				opt.DocumentCount = c.Documents.CountByTag(ctx, opt.Tag)
			}
			opt.Detail = optionDetail(opt)
			cq.Options = append(cq.Options, opt)
		}
		out = append(out, cq)
	}
	return out, nil
}

func (c *Clarifier) knowsTag(tag string) bool {
	for _, v := range c.Vocabulary {
		if strings.EqualFold(v, tag) {
			return true
		}
	}
	// No vocabulary configured ⇒ nothing to check against, so accept rather than
	// silently reject every option.
	return len(c.Vocabulary) == 0
}

// optionDetail renders the count an operator reads.
//
// "61 documents" is a decision; the same option without it is a preference. When
// the count is unavailable the detail says so rather than showing nothing, so a
// missing number never reads as zero.
func optionDetail(o ClarificationOption) string {
	switch {
	case o.DocumentCount < 0:
		return "document count unavailable"
	case o.DocumentCount == 0:
		return "0 documents — this option would search nothing"
	case o.DocumentCount == 1:
		return "1 document"
	default:
		return fmt.Sprintf("%d documents", o.DocumentCount)
	}
}

func normaliseKind(k string) string {
	if strings.EqualFold(strings.TrimSpace(k), KindJudgement) {
		return KindJudgement
	}
	return KindScope
}

func (c *Clarifier) buildPrompt(goal string) string {
	var b strings.Builder
	b.WriteString(`Decide whether a goal can be planned as written, or whether one decision is
missing that only the person asking can make.

Answer with questions ONLY when the answer would change what gets done. A goal
that is merely broad is still answerable — breadth is a plan's problem, not a
question. Vagueness that changes the OUTCOME is what to ask about.

Two kinds:
- "scope": which subset of the corpus. Options should name a classification tag,
  so the count of matching documents can be shown. Never invent a tag.
- "judgement": what counts as done. Options are descriptions, not tags.

For every question say what actually differs between the options in
why_it_changes_the_answer. A question without that reads as bureaucracy and gets
answered at random.

Reply with ONLY this JSON:
{"answerable":true,"reason":"","questions":[{"question":"","kind":"scope",
 "why_it_changes_the_answer":"","options":[{"label":"","tag":""}]}]}

`)
	if len(c.Vocabulary) > 0 {
		b.WriteString("CLASSIFICATION TAGS (the only tags you may name): " +
			strings.Join(c.Vocabulary, ", ") + "\n\n")
	} else {
		b.WriteString("No classification vocabulary is configured — ask judgement questions only.\n\n")
	}
	b.WriteString("GOAL: " + goal + "\n")
	return b.String()
}
