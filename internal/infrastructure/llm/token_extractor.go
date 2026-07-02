package llm

import "encoding/json"

// TokenUsage holds the token counts from an LLM response.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
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
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openaiFullResp struct {
	Usage openaiUsage `json:"usage"`
}

func (e *openaiExtractor) Extract(responseBody []byte) (TokenUsage, error) {
	var r openaiFullResp
	if err := json.Unmarshal(responseBody, &r); err != nil {
		return TokenUsage{}, nil
	}
	return TokenUsage{
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
	}, nil
}

// anthropicExtractor extracts token usage from Anthropic message responses.
type anthropicExtractor struct{}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicFullResp struct {
	Usage anthropicUsage `json:"usage"`
}

func (e *anthropicExtractor) Extract(responseBody []byte) (TokenUsage, error) {
	var r anthropicFullResp
	if err := json.Unmarshal(responseBody, &r); err != nil {
		return TokenUsage{}, nil
	}
	return TokenUsage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
	}, nil
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
