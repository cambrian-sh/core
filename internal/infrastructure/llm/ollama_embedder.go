package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type OllamaEmbedder struct {
	BaseURL   string
	Model     string
	TimeoutMs int
	// QueryPrefix is prepended by EmbedQuery only (ADR-0048). Document embeds via
	// Embed never get it, so an asymmetric-retrieval model (bge-large-en-v1.5)
	// gets the right instruction on the query side and a bare document on the
	// store side. Empty = symmetric (EmbedQuery == Embed).
	QueryPrefix string
}

var _ domain.BatchEmbedder = (*OllamaEmbedder)(nil)

// EmbedQuery embeds QUERY text, applying QueryPrefix when set. This is the recall
// side of an asymmetric embedder; the store side uses the plain Embed. A nil/empty
// prefix makes it identical to Embed.
func (e *OllamaEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if e.QueryPrefix != "" {
		text = e.QueryPrefix + text
	}
	return e.Embed(ctx, text)
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	timeout := time.Duration(e.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout}

	reqBody := ollamaEmbedRequest{Model: e.Model, Prompt: text}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/embeddings", e.BaseURL), &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: HTTP %d", resp.StatusCode)
	}

	var ollamaResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, err
	}

	vec := make([]float32, len(ollamaResp.Embedding))
	for i, val := range ollamaResp.Embedding {
		vec[i] = float32(val)
	}
	return vec, nil
}

type ollamaBatchEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaBatchEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// EmbedBatch vectorizes a slice of texts in a single Ollama /api/embed
// call using the batched `input` field (supported since Ollama 0.1.32) and
// returns the vectors in the same order as texts. TimeoutMs is applied to
// the whole batch (one HTTP request); callers with large batches should
// scale TimeoutMs accordingly.
func (e *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	timeout := time.Duration(e.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout}

	reqBody := ollamaBatchEmbedRequest{Model: e.Model, Input: texts}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return nil, err
	}

	// Ollama's BATCH endpoint is /api/embed (accepts `input` as an array and
	// returns `embeddings`); /api/embeddings is the legacy SINGLE endpoint that
	// only understands `prompt` and silently ignores `input` (returning 0
	// embeddings) — using it here was the "got 0 embeddings" bug.
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/embed", e.BaseURL), &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: HTTP %d", resp.StatusCode)
	}

	var ollamaResp ollamaBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, err
	}

	if len(ollamaResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embedder: batch dimension mismatch: got %d embeddings for %d inputs", len(ollamaResp.Embeddings), len(texts))
	}

	out := make([][]float32, len(ollamaResp.Embeddings))
	for i, emb := range ollamaResp.Embeddings {
		vec := make([]float32, len(emb))
		for j, val := range emb {
			vec[j] = float32(val)
		}
		out[i] = vec
	}
	return out, nil
}
