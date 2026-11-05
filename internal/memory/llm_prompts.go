package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// The memory organs' library prompts (ADR-0139 C4 / D2).
//
// Each organ call is two library prompts: `<id>`, the stable system block (no slots,
// cached), and `<id>_input`, the volatile user message whose slots carry the per-call
// data inside tagged regions. Their version is derived from both static hashes, so
// the version an organ writes (an extractor version, an assessment authority) changes
// whenever a byte of either file that is sent changes, and cannot certify text that
// was never sent.

// promptPair is one organ call's system and user-message prompts.
type promptPair struct {
	id      string
	system  prompts.Prompt
	input   prompts.Prompt
	version string
}

// loadPromptPair loads `<id>` and `<id>_input` from the embedded library.
func loadPromptPair(id string) (promptPair, error) {
	sys, err := prompts.Get(id)
	if err != nil {
		return promptPair{}, err
	}
	in, err := prompts.Get(id + "_input")
	if err != nil {
		return promptPair{}, err
	}
	if len(sys.Inputs) != 0 {
		return promptPair{}, fmt.Errorf("memory: prompt %s is the cached system block and must have no slots, has %v", id, sys.Inputs)
	}
	sum := sha256.Sum256([]byte(sys.StaticHash() + "\n" + in.StaticHash()))
	return promptPair{
		id: id, system: sys, input: in,
		version: fmt.Sprintf("%s.v%d.%s", id, sys.Version, hex.EncodeToString(sum[:])[:12]),
	}, nil
}

// mustPromptPair is loadPromptPair for the package-level prompts: the files are
// embedded and linted (prompts/lint_test.go), so a failure is a build defect, not a
// runtime condition.
func mustPromptPair(id string) promptPair {
	p, err := loadPromptPair(id)
	if err != nil {
		panic(fmt.Sprintf("memory: library prompt %s: %v", id, err))
	}
	return p
}

// request renders one call: the system prompt as one cached block, the input as the
// single user message, and the system prompt's response schema, if any.
func (p promptPair) request(values map[string]string) (domain.GenerateRequest, error) {
	r, err := p.input.Render(values)
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("memory: prompt %s: %w", p.input.ID, err)
	}
	req := domain.GenerateRequest{
		System:   []domain.SystemBlock{{Text: p.system.Body, Cache: true}},
		Messages: []domain.ModelMessage{domain.UserMessage(r.Text)},
	}
	if len(p.system.ResponseSchema) > 0 {
		req.ResponseSchema = &domain.ResponseSchema{Name: p.id, Schema: p.system.ResponseSchema}
	}
	return req, nil
}

// mustRequest is request for callers whose slot set is fixed in code: a slot mismatch
// is a defect the golden tests catch, never a property of the data.
func (p promptPair) mustRequest(values map[string]string) domain.GenerateRequest {
	req, err := p.request(values)
	if err != nil {
		panic(err)
	}
	return req
}

// requestChars is the size of a request as sent, for window budgets and call
// timeouts: everything a string-only generator would receive, schema included.
func requestChars(req domain.GenerateRequest) int { return len(domain.FlattenRequest(req)) }

var (
	factExtractPrompts = mustPromptPair("fact_extract")
	factAssessPrompts  = mustPromptPair("fact_assess")
	kgTripletsPrompts  = mustPromptPair("kg_triplets")
)
