package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ollamaStreamChunk is the JSON shape Ollama returns for each streaming line.
type ollamaStreamChunk struct {
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	EvalCount       int    `json:"eval_count"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	TotalDuration   int    `json:"total_duration"`
}

// GenerateStream opens a streaming generation call to the Ollama /api/generate endpoint.
// Chunks are sent on the returned channel; the caller must drain it.
// The channel is buffered (capacity 64) and is closed when the stream ends or on error.
func (c *OllamaClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	// No total-duration timeout on a STREAMING call: http.Client.Timeout caps the
	// entire request including body reads, which would kill a slow model mid-stream
	// regardless of the actual step budget. Cancellation is governed by ctx (the
	// step/RPC deadline) via http.NewRequestWithContext + the ctx.Done() check in the
	// read loop below. For the interview path ctx carries no deadline, so the agent's
	// LLM call runs to completion ("remove the timeout for agents' llm calls").
	httpClient := &http.Client{}

	reqBody := ollamaRequest{Model: c.Model, Prompt: prompt, Stream: true, Options: ollamaOptions{Temperature: 0}}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/generate", c.BaseURL), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
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
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var streamChunk ollamaStreamChunk
			if err := json.Unmarshal(line, &streamChunk); err != nil {
				continue
			}
			chunk := domain.StreamChunk{
				Text: streamChunk.Response,
			}
			if streamChunk.Done {
				chunk.IsFinal = true
				// Ollama includes eval counts only when done=true
				if streamChunk.EvalCount > 0 {
					chunk.UsageCompletionTokens = streamChunk.EvalCount
					chunk.UsagePromptTokens = streamChunk.PromptEvalCount
					chunk.UsageTotalTokens = streamChunk.PromptEvalCount + streamChunk.EvalCount
				}
				ch <- chunk
				return
			}
			ch <- chunk
		}
		// If scanner exited without done=true (error or EOF), send empty final chunk
		// so the caller knows the stream ended.
		if err := scanner.Err(); err != nil && err != io.EOF {
			ch <- domain.StreamChunk{IsFinal: true, Text: fmt.Sprintf("stream_error: %v", err)}
		} else {
			ch <- domain.StreamChunk{IsFinal: true}
		}
	}()

	return ch, nil
}
