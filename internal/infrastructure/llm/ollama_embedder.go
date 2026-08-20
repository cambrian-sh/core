package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cambrian-sh/core/domain"
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
	// MaxInputRunes caps the input length before it is sent to Ollama. A BERT embedder
	// (bge-large: ~512-token window) returns HTTP 500 — "the input length exceeds the
	// context length" — when handed anything longer, which is exactly what happened when
	// whole tool outputs / large documents were embedded un-chunked. Truncating here is the
	// safety net: an embedding of the first N runes is still useful, and it can never crash
	// the memory write/recall path. 0 ⇒ defaultMaxEmbedRunes.
	MaxInputRunes int
}

// defaultMaxEmbedRunes is a generous first-pass cap for a 512-token model (bge-large accepts
// ~500 tokens ≈ 3000 chars of prose). It is deliberately not conservative: a too-long input
// is caught and retried shorter by Embed's overflow loop, so this only needs to make the
// common case a single request.
const defaultMaxEmbedRunes = 2000

// truncateForEmbed caps text at the embedder's rune budget, on a rune boundary.
func (e *OllamaEmbedder) truncateForEmbed(text string) string {
	limit := e.MaxInputRunes
	if limit <= 0 {
		limit = defaultMaxEmbedRunes
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return string(r[:limit])
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
	// A rune cap alone is not enough: a BERT tokenizer turns dense/degenerate input (long
	// runs, code, non-English) into far more tokens per rune, so a cap safe for prose still
	// overflows the 512-token window. So: cap first (cheap), then on a context-overflow 500
	// halve and retry down to a floor. Natural text embeds on the first try; pathological
	// input shrinks until it fits, instead of failing the whole memory write/recall.
	runes := []rune(e.truncateForEmbed(text))
	for {
		vec, overflow, err := e.embedOnce(ctx, string(runes))
		if err == nil {
			return vec, nil
		}
		if !overflow || len(runes) <= minEmbedRunes {
			return nil, err
		}
		runes = runes[:len(runes)/2]
	}
}

// minEmbedRunes is the floor the retry-halving stops at; below this the input is small
// enough that a persistent overflow is some other problem, so we surface the error.
const minEmbedRunes = 256

// embedOnce does one embed request. overflow is true when Ollama rejected the input for
// exceeding the model's context window (so the caller can retry with a shorter input).
func (e *OllamaEmbedder) embedOnce(ctx context.Context, text string) (vec []float32, overflow bool, err error) {
	timeout := time.Duration(e.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout, Transport: sharedLLMTransport}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ollamaEmbedRequest{Model: e.Model, Prompt: text}); err != nil {
		return nil, false, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/embeddings", e.BaseURL), &buf)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, isContextOverflow(body), fmt.Errorf("embedder: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var ollamaResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, false, err
	}
	vec = make([]float32, len(ollamaResp.Embedding))
	for i, val := range ollamaResp.Embedding {
		vec[i] = float32(val)
	}
	return vec, false, nil
}

// isContextOverflow detects Ollama's "the input length exceeds the context length" 500 so a
// too-long embed can be retried shorter rather than dropped.
func isContextOverflow(body []byte) bool {
	b := bytes.ToLower(body)
	return bytes.Contains(b, []byte("context length")) || bytes.Contains(b, []byte("exceeds the context")) || bytes.Contains(b, []byte("input length"))
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

	// Cap every input, then retry the whole batch with a shorter per-input cap if Ollama
	// rejects it for context overflow (one oversized member 500s the batch). Same reasoning
	// as Embed: a rune cap alone can't guarantee a token count under the window.
	cap := e.MaxInputRunes
	if cap <= 0 {
		cap = defaultMaxEmbedRunes
	}
	startCap := cap
	for {
		vecs, overflow, err := e.embedBatchOnce(ctx, texts, cap)
		if err == nil {
			if cap < startCap {
				// One oversized member forced the retry, but the halved cap was
				// applied to EVERY text in the batch — all their embeddings now
				// cover only a prefix. Silent before; permanent (the stored text
				// keeps the full body, only the vector is a prefix), so it must
				// be visible in the ingest log.
				slog.Warn("OllamaEmbedder: batch embedded at a REDUCED input cap — every vector in this batch covers a prefix only",
					"cap_runes", cap, "start_cap_runes", startCap, "batch_size", len(texts))
			}
			return vecs, nil
		}
		if !overflow || cap <= minEmbedRunes {
			return nil, err
		}
		cap /= 2
	}
}

// embedBatchOnce does one batch embed with each input capped to maxRunes.
func (e *OllamaEmbedder) embedBatchOnce(ctx context.Context, texts []string, maxRunes int) (vecs [][]float32, overflow bool, err error) {
	timeout := time.Duration(e.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout, Transport: sharedLLMTransport}

	capped := make([]string, len(texts))
	for i, t := range texts {
		r := []rune(t)
		if len(r) > maxRunes {
			r = r[:maxRunes]
		}
		capped[i] = string(r)
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ollamaBatchEmbedRequest{Model: e.Model, Input: capped}); err != nil {
		return nil, false, err
	}

	// Ollama's BATCH endpoint is /api/embed (accepts `input` as an array and
	// returns `embeddings`); /api/embeddings is the legacy SINGLE endpoint that
	// only understands `prompt` and silently ignores `input` (returning 0
	// embeddings) — using it here was the "got 0 embeddings" bug.
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/embed", e.BaseURL), &buf)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, isContextOverflow(body), fmt.Errorf("embedder: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var ollamaResp ollamaBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, false, err
	}
	if len(ollamaResp.Embeddings) != len(texts) {
		return nil, false, fmt.Errorf("embedder: batch dimension mismatch: got %d embeddings for %d inputs", len(ollamaResp.Embeddings), len(texts))
	}

	out := make([][]float32, len(ollamaResp.Embeddings))
	for i, emb := range ollamaResp.Embeddings {
		vec := make([]float32, len(emb))
		for j, val := range emb {
			vec[j] = float32(val)
		}
		out[i] = vec
	}
	return out, false, nil
}
