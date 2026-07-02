package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type OllamaClient struct {
	BaseURL   string
	Model     string
	TimeoutMs int
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
}

type ollamaRequest struct {
	Model   string        `json:"model"`
	Prompt  string        `json:"prompt"`
	Stream  bool          `json:"stream"`
	Format  string        `json:"format"`
	Options ollamaOptions `json:"options"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

func (c *OllamaClient) Generate(ctx context.Context, prompt string) (string, error) {
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout}

	reqBody := ollamaRequest{Model: c.Model, Prompt: prompt, Stream: false, Format: "json", Options: ollamaOptions{Temperature: 0}}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/generate", c.BaseURL), bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// ADR-0042 D4: surface real failures instead of swallowing them. An Ollama
	// error body (e.g. {"error":"model not found"}, HTTP 404) unmarshals cleanly
	// into an empty Response — previously returned as ("", nil), producing a
	// silent empty success that broke callers with "unexpected end of JSON input"
	// three layers up. Check status and reject empty responses so the health
	// decorator sees a genuine failure.
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var oResp ollamaResponse
	if err := json.Unmarshal(body, &oResp); err != nil {
		return "", err
	}
	if strings.TrimSpace(oResp.Response) == "" {
		return "", fmt.Errorf("ollama: empty response from model %q", c.Model)
	}
	return oResp.Response, nil
}
