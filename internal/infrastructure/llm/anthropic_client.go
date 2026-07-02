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

type AnthropicClient struct {
	Endpoint  string
	Model     string
	APIKeyEnv string
	TimeoutMs int
}

type anthropicMessageReq struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []anthropicMsg `json:"messages"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicMessageResp struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (c *AnthropicClient) Generate(ctx context.Context, prompt string) (string, error) {
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout}

	reqBody := anthropicMessageReq{
		Model:     c.Model,
		MaxTokens: 4096,
		Messages: []anthropicMsg{
			{Role: "user", Content: prompt},
		},
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/messages", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
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
		return "", fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var msgResp anthropicMessageResp
	if err := json.Unmarshal(body, &msgResp); err != nil {
		return "", err
	}
	if len(msgResp.Content) == 0 {
		return "", fmt.Errorf("anthropic: no content in response")
	}
	return msgResp.Content[0].Text, nil
}
