package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type OpenAIClient struct {
	Endpoint  string
	Model     string
	APIKeyEnv string
	TimeoutMs int
	// DisableThinking sends thinking:{"type":"disabled"} to suppress server-side
	// reasoning on OpenAI-compat reasoning models (deepseek-v4-flash on opencode).
	DisableThinking bool
}

// openAIThinking is the opencode/deepseek reasoning toggle. Type "disabled"
// suppresses reasoning-token generation (faster; no empty-content-from-budget).
type openAIThinking struct {
	Type string `json:"type"`
}

func disabledThinking(disable bool) *openAIThinking {
	if !disable {
		return nil
	}
	return &openAIThinking{Type: "disabled"}
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIChatMsg `json:"messages"`
	Thinking *openAIThinking `json:"thinking,omitempty"`
}

type openAIChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *OpenAIClient) Generate(ctx context.Context, prompt string) (string, error) {
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout, Transport: sharedLLMTransport}

	reqBody := openAIChatRequest{
		Model: c.Model,
		Messages: []openAIChatMsg{
			{Role: "user", Content: prompt},
		},
		Thinking: disabledThinking(c.DisableThinking),
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/chat/completions", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", err
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices in response")
	}
	return chatResp.Choices[0].Message.Content, nil
}
