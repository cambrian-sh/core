package awareness

import (
	"context"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
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

	// A prompt that does not render is a build defect (the prompt lint catches
	// it before release), not a model failure, so it is returned rather than
	// swallowed by the fail-open below.
	req, err := c.buildRequest(goal)
	if err != nil {
		return nil, err
	}
	resp, err := domain.GenerateVia(ctx, c.LLM, req)
	if err != nil {
		// Fail OPEN. The clarifier's failure must not block proposing: the
		// operator still gets a plan to inspect, which is strictly better than an
		// error page.
		return nil, nil
	}

	// The reply was validated against prompts/clarifier/clarification.schema.json
	// by the provider layer (C1); decodeReply tolerates only a code fence on an
	// unchecked reply. Undecodable is fail-open, like a model error.
	var m modelClarification
	if decodeReply(resp, &m) != nil {
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

// clarifierPrompts is the clarifier's library prompt pair: prompts/clarifier
// (the stable system block; v2 moved the data regions out of it) and
// prompts/clarifier_input (the vocabulary and, last, the goal, as tagged data).
// The text, its response schema and its static hash live there; this file only
// fills the slots.
var clarifierPrompts = prompts.MustPair("clarifier", "clarifier_input")

// clarifierDataTags are the data regions of clarifier_input. A closing </goal>
// inside the operator's text would otherwise end the data region early and let
// the rest of the goal read as instructions.
var clarifierDataTags = []string{"classification_tags", "goal"}

// clarifierCacheTTL: clarification runs when an operator proposes a plan, which is
// human-paced, so the long breakpoint lifetime (ADR-0139 D1).
const clarifierCacheTTL = domain.CacheTTLLong

// buildRequest renders the clarifier call. The goal is untrusted operator text
// and sits in a <goal> data region, last (ADR-0139 D2); the vocabulary is kernel
// configuration, also data. An empty vocabulary renders an empty
// <classification_tags> region, which the prompt reads as "ask judgement
// questions only".
func (c *Clarifier) buildRequest(goal string) (domain.GenerateRequest, error) {
	return planRequest(clarifierPrompts, clarifierDataTags, map[string]string{
		"classification_tags": strings.Join(c.Vocabulary, ", "),
		"goal":                strings.TrimSpace(goal),
	}, clarifierCacheTTL)
}
