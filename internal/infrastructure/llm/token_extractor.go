package llm

import "encoding/json"

// TokenUsage holds the token counts from an LLM response.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens is the part of PromptTokens the provider served from its
	// prompt cache. Already included in PromptTokens (ADR-0131 D2).
	CacheReadTokens int
	// Estimated is true when the counts come from the len/4 fallback because the
	// provider returned no usage (ADR-0131 D2: real usage first, estimate only as
	// a flagged fallback). A sum is Estimated when any of its parts is.
	Estimated bool
	// BudgetOverrun is set by the gateway's Complete when its Pass-1 cut-off fired
	// for the lease (ADR-0018). Providers never set it.
	BudgetOverrun bool
}

// IsZero reports whether u carries no token counts at all — the shape a provider
// response without a usage block parses to.
func (u TokenUsage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0
}

// Add returns the field-wise sum of u and v. Estimated is sticky; BudgetOverrun
// is OR-ed.
func (u TokenUsage) Add(v TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + v.PromptTokens,
		CompletionTokens: u.CompletionTokens + v.CompletionTokens,
		TotalTokens:      u.TotalTokens + v.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens + v.CacheReadTokens,
		Estimated:        u.Estimated || v.Estimated,
		BudgetOverrun:    u.BudgetOverrun || v.BudgetOverrun,
	}
}

// EstimateUsage is the len/4 fallback for one call, flagged Estimated. It is the
// one place an estimated usage row is built, so "estimated" always means the
// same thing (ADR-0131 D2).
func EstimateUsage(prompt, response string) TokenUsage {
	in, out := EstimateTokens(prompt), EstimateTokens(response)
	return TokenUsage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out, Estimated: true}
}

// TokenUsageExtractor parses an LLM provider's HTTP response body
// and extracts token usage counts.
type TokenUsageExtractor interface {
	Extract(responseBody []byte) (TokenUsage, error)
}

// ollamaExtractor extracts token usage from Ollama /api/generate responses.
type ollamaExtractor struct{}

type ollamaGenResponse struct {
	PromptEvalCount    int `json:"prompt_eval_count"`
	EvalCount          int `json:"eval_count"`
	PromptEvalDuration int `json:"prompt_eval_duration"`
	EvalDuration       int `json:"eval_duration"`
	TotalDuration      int `json:"total_duration"`
}

func (e *ollamaExtractor) Extract(responseBody []byte) (TokenUsage, error) {
	var r ollamaGenResponse
	if err := json.Unmarshal(responseBody, &r); err != nil {
		return TokenUsage{}, nil // non-parseable = no token data
	}
	return TokenUsage{
		PromptTokens:     r.PromptEvalCount,
		CompletionTokens: r.EvalCount,
		TotalTokens:      r.PromptEvalCount + r.EvalCount,
	}, nil
}

// openaiExtractor extracts token usage from OpenAI chat completion responses.
type openaiExtractor struct{}

type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// tokenUsage converts the OpenAI usage block. prompt_tokens already includes the
// cached tokens, so CacheReadTokens is a breakdown, not an addition.
func (u openaiUsage) tokenUsage() TokenUsage {
	out := TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

type openaiFullResp struct {
	Usage openaiUsage `json:"usage"`
}

func (e *openaiExtractor) Extract(responseBody []byte) (TokenUsage, error) {
	var r openaiFullResp
	if err := json.Unmarshal(responseBody, &r); err != nil {
		return TokenUsage{}, nil
	}
	return r.Usage.tokenUsage(), nil
}

// anthropicExtractor extracts token usage from Anthropic message responses.
type anthropicExtractor struct{}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// tokenUsage converts the Anthropic usage block. Anthropic's input_tokens
// EXCLUDES cache reads and cache writes, so the prompt total is the sum of all
// three — the same "every input token" meaning PromptTokens has for OpenAI.
func (u anthropicUsage) tokenUsage() TokenUsage {
	in := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return TokenUsage{
		PromptTokens:     in,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      in + u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
	}
}

type anthropicFullResp struct {
	Usage anthropicUsage `json:"usage"`
}

func (e *anthropicExtractor) Extract(responseBody []byte) (TokenUsage, error) {
	var r anthropicFullResp
	if err := json.Unmarshal(responseBody, &r); err != nil {
		return TokenUsage{}, nil
	}
	return r.Usage.tokenUsage(), nil
}

// CalculateCost computes the estimated cost of a model call.
// inputCostPer1M is the cost per 1 million input/prompt tokens.
// outputCostPer1M is the cost per 1 million output/completion tokens.
func CalculateCost(usage TokenUsage, inputCostPer1M, outputCostPer1M float64) float64 {
	inputCost := (float64(usage.PromptTokens) / 1_000_000.0) * inputCostPer1M
	outputCost := (float64(usage.CompletionTokens) / 1_000_000.0) * outputCostPer1M
	return inputCost + outputCost
}

const charsPerToken = 4

// EstimateTokens returns a rough token count for a text string.
// Uses the heuristic 4 characters ≈ 1 token.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	tokens := len(text) / charsPerToken
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// EstimateCostFromText returns an estimated cost for the given prompt and
// response text using the provided per-1M pricing.
func EstimateCostFromText(prompt, response string, inputCostPer1M, outputCostPer1M float64) float64 {
	promptTokens := EstimateTokens(prompt)
	responseTokens := EstimateTokens(response)
	return CalculateCost(TokenUsage{PromptTokens: promptTokens, CompletionTokens: responseTokens, TotalTokens: promptTokens + responseTokens}, inputCostPer1M, outputCostPer1M)
}
