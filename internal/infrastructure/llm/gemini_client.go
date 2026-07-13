package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// GeminiClient is the native Google Gemini client. It uses the standard
// `generateContent` endpoint (POST /v1beta/models/{model}:generateContent)
// and authenticates via the `x-goog-api-key` header. Reference:
// https://ai.google.dev/gemini-api/docs/text-generation
//
// Request body shape:
//   {"contents": [{"parts": [{"text": "<prompt>"}]}], "generationConfig": {...}}
//
// Response body shape:
//   {"candidates": [{"content": {"parts": [{"text": "<answer>"}]}}],
//    "usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": M, "totalTokenCount": K}}
type GeminiClient struct {
	Endpoint  string // default: https://generativelanguage.googleapis.com/v1beta
	Model     string // e.g. "gemini-2.0-flash"; put in the URL path, NOT the body
	APIKeyEnv string // env var name holding the API key (default: GEMINI_API_KEY)
	TimeoutMs int
}

// geminiGenerateRequest is the body shape for generateContent. The model
// goes in the URL path, not here.
type geminiGenerateRequest struct {
	Contents []geminiContent `json:"contents"`
	// generationConfig is optional; the API accepts requests without it.
	// We leave it unset to use the model's defaults.
	GenerationConfig *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
	// Role is optional; default is "user".
	Role string `json:"role,omitempty"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	TopP            float64 `json:"topP,omitempty"`
}

// geminiGenerateResponse is the non-streaming response.
type geminiGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

// geminiStreamChunk is one chunk in the streaming response. The API returns
// JSON Lines (one JSON object per line) when alt=sse is set, otherwise a
// stream of [object]JSON chunks. We use streamGenerateContent which returns
// an array-style stream (each chunk is a complete JSON object on its own line).
type geminiStreamChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func (c *GeminiClient) endpoint() string {
	if c.Endpoint == "" {
		return "https://generativelanguage.googleapis.com/v1beta"
	}
	return strings.TrimRight(c.Endpoint, "/")
}

func (c *GeminiClient) apiKey() string {
	env := c.APIKeyEnv
	if env == "" {
		env = "GEMINI_API_KEY"
	}
	return os.Getenv(env)
}

func (c *GeminiClient) modelURL() string {
	// The model name goes in the URL path. The Gemini API accepts both
	// "gemini-2.0-flash" and "models/gemini-2.0-flash" — strip the prefix
	// if present to avoid double "models/models/".
	model := strings.TrimPrefix(c.Model, "models/")
	return fmt.Sprintf("%s/models/%s", c.endpoint(), model)
}

// Generate sends a non-streaming generateContent request and returns the
// concatenated text from the first candidate.
func (c *GeminiClient) Generate(ctx context.Context, prompt string) (string, error) {
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	httpClient := &http.Client{Timeout: timeout}

	body := geminiGenerateRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: prompt},
				},
				Role: "user",
			},
		},
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	url := c.modelURL() + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := c.apiKey(); key != "" {
		req.Header.Set("x-goog-api-key", key)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed geminiGenerateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("gemini: parse response: %w (body: %s)", err, truncate(string(respBody), 512))
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: empty response (body: %s)", truncate(string(respBody), 512))
	}
	return parsed.Candidates[0].Content.Parts[0].Text, nil
}

// GenerateStream sends a streaming generateContent request and emits text
// chunks on a buffered channel. The channel is closed when the stream ends.
func (c *GeminiClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	// No total-duration timeout on a STREAMING call: http.Client.Timeout caps
	// the ENTIRE request including body reads. Cancellation is via ctx.
	httpClient := &http.Client{}

	body := geminiGenerateRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: prompt},
				},
				Role: "user",
			},
		},
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := c.modelURL() + ":streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := c.apiKey(); key != "" {
		req.Header.Set("x-goog-api-key", key)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan domain.StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		// alt=sse gives us SSE framing: lines like "data: {...}\n\n". We
		// strip the "data: " prefix and parse the JSON.
		reader := newLineReader(resp.Body)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line, ok := reader.next()
			if !ok {
				ch <- domain.StreamChunk{IsFinal: true}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				line = strings.TrimPrefix(line, "data: ")
			}
			if line == "[DONE]" {
				ch <- domain.StreamChunk{IsFinal: true}
				return
			}
			var chunk geminiStreamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// Skip malformed lines.
				continue
			}
			for _, cand := range chunk.Candidates {
				for _, part := range cand.Content.Parts {
					if part.Text != "" {
						ch <- domain.StreamChunk{Text: part.Text}
					}
				}
				if cand.FinishReason == "STOP" || cand.FinishReason == "MAX_TOKENS" {
					ch <- domain.StreamChunk{IsFinal: true}
					return
				}
			}
		}
	}()
	return ch, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// lineReader yields newline-terminated lines from an io.Reader. Used for
// parsing the SSE-framed streaming response from Gemini's streamGenerateContent
// endpoint. Mirrors the helper in the OpenAI/Anthropic/Ollama streamers.
type lineReader struct {
	r   io.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, buf: make([]byte, 0, 4096)}
}

func (l *lineReader) next() (string, bool) {
	for {
		// Look for a newline in the buffer.
		for i := 0; i < len(l.buf); i++ {
			if l.buf[i] == '\n' {
				line := string(l.buf[:i])
				l.buf = l.buf[i+1:]
				return line, true
			}
		}
		// Read more.
		tmp := make([]byte, 4096)
		n, err := l.r.Read(tmp)
		if n > 0 {
			l.buf = append(l.buf, tmp[:n]...)
		}
		if err != nil {
			// EOF or other error: return whatever's in the buffer (if any),
			// then signal end of stream.
			if len(l.buf) > 0 {
				line := string(l.buf)
				l.buf = l.buf[:0]
				return line, true
			}
			return "", false
		}
	}
}

// geminiExtractor extracts token usage from a Gemini response body.
// The API returns usageMetadata with promptTokenCount and candidatesTokenCount.
type geminiExtractor struct{}

func (geminiExtractor) Extract(body []byte) (TokenUsage, error) {
	var parsed geminiGenerateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return TokenUsage{}, err
	}
	return TokenUsage{
		PromptTokens:     parsed.UsageMetadata.PromptTokenCount,
		CompletionTokens: parsed.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      parsed.UsageMetadata.TotalTokenCount,
	}, nil
}
