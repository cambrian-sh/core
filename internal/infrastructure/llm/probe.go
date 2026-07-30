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

	"github.com/cambrian-sh/core/internal/config"
)

// ProbeResult is one live diagnostic call against a generator's endpoint.
type ProbeResult struct {
	OK bool
	// ModelServed is the model string the ENDPOINT echoed back in its response.
	//
	// This is the field the probe exists for. An endpoint that accepts a request
	// for one model and answers with another is the most common misconfiguration
	// in this space, and it is invisible everywhere else — including in a
	// perfectly successful generation, which is exactly why a normal call cannot
	// substitute for this one. Empty ⇒ the provider does not echo a model back.
	ModelServed string
	// ModelRequested is what the probe asked the endpoint for — the generator's
	// configured model. Reported so the comparison has both sides.
	ModelRequested string
	Sample         string
	LatencyMs      int64
	Err            string
}

// ProbeGenerator makes ONE real call against a generator's configured endpoint
// and reports what came back.
//
// It deliberately does NOT go through domain.Generator. That interface returns
// text and nothing else, so surfacing the served model through it would mean
// changing the one path every generation in the kernel flows through — a large
// blast radius for a diagnostic. Issuing the request directly keeps the
// generation path untouched and lets the probe read fields the interface throws
// away.
func ProbeGenerator(ctx context.Context, g config.GeneratorConfig) ProbeResult {
	timeout := time.Duration(g.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var served, sample string
	var err error

	switch strings.ToLower(g.Provider) {
	case "ollama":
		served, sample, err = probeOllama(ctx, g)
	default:
		// openai, opencode, groq, together, deepseek and every other
		// OpenAI-compatible endpoint.
		served, sample, err = probeOpenAICompat(ctx, g)
	}

	res := ProbeResult{
		ModelServed:    served,
		ModelRequested: g.Model,
		Sample:         truncateSample(sample),
		LatencyMs:      time.Since(start).Milliseconds(),
	}
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.OK = true
	return res
}

// probePrompt is deliberately trivial and deterministic: the probe is testing
// reachability, credentials and model identity, not model quality. A long prompt
// would spend tokens to learn nothing extra.
const probePrompt = "Reply with the single word: ok"

func probeOpenAICompat(ctx context.Context, g config.GeneratorConfig) (served, sample string, err error) {
	body, _ := json.Marshal(map[string]any{
		"model": g.Model,
		"messages": []map[string]string{
			{"role": "user", "content": probePrompt},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/chat/completions", strings.TrimRight(g.Endpoint, "/")),
		bytes.NewBuffer(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// The SAME resolution the real clients use. When the probe read only the
	// environment it reported "the key was rejected" for a credential the
	// console had stored and displayed -- diagnosing a fault in the key rather
	// than in the thing that never looked for it.
	if key := APIKeyFor(g.ID, g.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		// The body carries the actionable half — "invalid api key", "model not
		// found" — so it travels rather than just the status code.
		return "", "", fmt.Errorf("endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("unparseable response: %w", err)
	}
	if len(parsed.Choices) > 0 {
		sample = parsed.Choices[0].Message.Content
	}
	return parsed.Model, sample, nil
}

func probeOllama(ctx context.Context, g config.GeneratorConfig) (served, sample string, err error) {
	body, _ := json.Marshal(map[string]any{
		"model":  g.Model,
		"prompt": probePrompt,
		"stream": false,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/generate", strings.TrimRight(g.Endpoint, "/")),
		bytes.NewBuffer(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		Model    string `json:"model"`
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("unparseable response: %w", err)
	}
	return parsed.Model, parsed.Response, nil
}

// truncateSample keeps enough for a human to see the endpoint answered rather
// than echoed, and no more. The full completion is not the operator's business
// here and would be one more place a model's output lands in a log.
func truncateSample(s string) string {
	s = strings.TrimSpace(s)
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
