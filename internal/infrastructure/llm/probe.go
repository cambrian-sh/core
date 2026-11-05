package llm

import (
	"context"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
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
// It goes through the generator's own client (NewClient, the one constructor)
// but NOT through the Provider stack: a diagnostic must not trip the breaker, be
// refused at a spend ceiling, or fall back to a different generator and report
// that one's health. Since ADR-0139 C1 the client returns the served model
// (GenerateResponse.Model), so the probe no longer needs its own hand-written
// request per provider — which also means Anthropic and Gemini generators are
// probed with their real API instead of an OpenAI-shaped request.
func ProbeGenerator(ctx context.Context, g config.GeneratorConfig) ProbeResult {
	timeout := time.Duration(g.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res := ProbeResult{ModelRequested: g.Model}
	if _, err := NewClient(g); err != nil {
		// An unrecognised provider string is still most likely OpenAI-compatible
		// (opencode, groq, together, ...): probe it as one, as before.
		g.Provider = "openai"
	}
	client, err := NewClient(g)
	if err == nil {
		var resp domain.GenerateResponse
		resp, err = client.GenerateRequest(ctx, domain.UserPrompt(probePrompt))
		res.ModelServed, res.Sample = resp.Model, truncateSample(resp.Text)
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.OK = true
	return res
}

// probePrompt asks for a reply short enough that the probe costs nothing.
const probePrompt = "Reply with the single word: ok"

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
