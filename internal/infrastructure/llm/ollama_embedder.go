package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
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

func (e *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for i, text := range texts {
		i, text := i, text
		g.Go(func() error {
			vec, err := e.Embed(ctx, text)
			if err != nil {
				return fmt.Errorf("embedding error for text %d: %w", i, err)
			}
			results[i] = vec
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
