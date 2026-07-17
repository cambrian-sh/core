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

// openaiStreamReq includes stream=true in the body.
type openaiStreamReq struct {
	Model         string          `json:"model"`
	Messages      []openAIChatMsg `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Thinking *openAIThinking `json:"thinking,omitempty"`
}

// openaiStreamDelta is the JSON shape per streaming SSE data line.
type openaiStreamDelta struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage"`
}

// GenerateStream opens a streaming chat completion call to the OpenAI endpoint.
// Chunks are sent on the returned buffered channel; it is closed when the stream ends.
func (c *OpenAIClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	// No total-duration timeout on a STREAMING call: http.Client.Timeout caps the
	// ENTIRE request including body reads, which kills a slow model mid-stream
	// regardless of the step budget — surfacing as "stream_error: context deadline
	// exceeded (Client.Timeout...)". Cancellation is governed by ctx (via
	// NewRequestWithContext + the ctx.Done() check in the read loop), mirroring the
	// Ollama streamer. The non-streaming OpenAIClient.Generate keeps its timeout.
	httpClient := &http.Client{Transport: sharedLLMTransport}

	reqBody := openaiStreamReq{
		Model: c.Model,
		Messages: []openAIChatMsg{
			{Role: "user", Content: prompt},
		},
		Stream: true,
		StreamOptions: &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true},
		Thinking: disabledThinking(c.DisableThinking),
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/chat/completions", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(body))
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
			if data == "[DONE]" {
				ch <- domain.StreamChunk{IsFinal: true}
				return
			}
			var delta openaiStreamDelta
			if err := json.Unmarshal([]byte(data), &delta); err != nil {
				continue
			}
			chunk := domain.StreamChunk{}
			if len(delta.Choices) > 0 {
				chunk.Text = delta.Choices[0].Delta.Content
				if delta.Choices[0].FinishReason != nil {
					chunk.IsFinal = true
				}
			}
			if delta.Usage != nil {
				chunk.IsFinal = true
				chunk.UsagePromptTokens = delta.Usage.PromptTokens
				chunk.UsageCompletionTokens = delta.Usage.CompletionTokens
				chunk.UsageTotalTokens = delta.Usage.TotalTokens
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
