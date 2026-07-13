package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// anthropicStreamReq adds stream=true.
type anthropicStreamReq struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []anthropicMsg `json:"messages"`
	Stream    bool           `json:"stream"`
}

// anthropicStreamEvent is the SSE event from Anthropic streaming.
type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Index *int   `json:"index"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
	Usage *anthropicUsage `json:"usage"`
}

// GenerateStream opens a streaming message call to the Anthropic Messages endpoint.
func (c *AnthropicClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	// No total-duration timeout on a STREAMING call: http.Client.Timeout caps the
	// ENTIRE request including body reads, killing a slow model mid-stream. Governed
	// by ctx instead, mirroring the Ollama/OpenAI streamers.
	httpClient := &http.Client{}

	reqBody := anthropicStreamReq{
		Model:     c.Model,
		MaxTokens: 4096,
		Messages: []anthropicMsg{
			{Role: "user", Content: prompt},
		},
		Stream: true,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/messages", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan domain.StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			var evt anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				continue
			}
			chunk := domain.StreamChunk{}
			switch evt.Type {
			case "content_block_delta":
				if evt.Delta != nil {
					chunk.Text = evt.Delta.Text
				}
			case "message_delta":
				if evt.Usage != nil {
					chunk.UsagePromptTokens = evt.Usage.InputTokens
					chunk.UsageCompletionTokens = evt.Usage.OutputTokens
					chunk.UsageTotalTokens = evt.Usage.InputTokens + evt.Usage.OutputTokens
				}
				if evt.Delta != nil && evt.Delta.StopReason != "" {
					chunk.IsFinal = true
				}
			case "message_stop":
				chunk.IsFinal = true
			default:
				// content_block_start, ping — ignore
				continue
			}
			ch <- chunk
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			ch <- domain.StreamChunk{IsFinal: true, Text: fmt.Sprintf("stream_error: %v", err)}
		} else {
			ch <- domain.StreamChunk{IsFinal: true}
		}
	}()

	return ch, nil
}
