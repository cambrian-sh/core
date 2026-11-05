package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"

	"github.com/cambrian-sh/core/domain"
)

// The provider-neutral half of the C1 pathway (ADR-0139 D1): capability gating,
// error normalisation, and the shims that keep the pre-C1 ports working. Each
// adapter maps a prepared domain.GenerateRequest onto its SDK and back; nothing
// here knows a provider's wire shape.

// prepareRequest applies a generator's declared Capabilities to req BEFORE it is
// mapped onto a wire request. A field the generator cannot honour is removed and
// named in the returned ignored list — never dropped silently — and becomes
// domain.ErrUnsupported only when the request marked it strict:
//
//   - Options.Strict for max_tokens, temperature and stop: strict means ENFORCED, so a
//     best-effort level is refused too (the caller asked for a guarantee);
//   - ResponseSchema.Strict for constrained decoding: same rule;
//   - tools are never ignorable. A tool request served by a model that cannot call
//     tools yields a model that never calls anything, which reads as a model-quality
//     problem (ADR-0097 D2), so it is always an error.
//
// The returned request is a shallow copy; the caller's slices are not mutated.
func prepareRequest(req domain.GenerateRequest, caps domain.Capabilities) (domain.GenerateRequest, []string, error) {
	var ignored []string
	ignore := func(f string) { ignored = append(ignored, f) }

	if len(req.Tools) > 0 && !caps.Tools.Sent() {
		return req, nil, domain.UnsupportedField(domain.FieldTools, "generator has no native tool-calling (native_tools)")
	}

	if len(req.System) > 0 && !caps.SystemRole.Sent() {
		// Folded into the first user turn: the text still reaches the model, but
		// without system-role authority, which is a real degradation — reported.
		req.Messages = foldSystemIntoMessages(req.System, req.Messages)
		req.System = nil
		ignore(domain.FieldSystem)
	}

	if req.ToolChoice.Mode != domain.ToolChoiceAuto && (len(req.Tools) == 0 || !caps.ToolChoice.Sent()) {
		if len(req.Tools) > 0 {
			ignore(domain.FieldToolChoice)
		}
		req.ToolChoice.Mode, req.ToolChoice.Name = domain.ToolChoiceAuto, ""
	}
	if req.ToolChoice.DisableParallel && (len(req.Tools) == 0 || !caps.ParallelTools.Sent()) {
		if len(req.Tools) > 0 {
			ignore(domain.FieldParallelTools)
		}
		req.ToolChoice.DisableParallel = false
	}

	if rs := req.ResponseSchema; rs != nil {
		if rs.Strict && caps.ResponseSchema != domain.LevelEnforced {
			return req, nil, domain.UnsupportedField(domain.FieldResponseSchema,
				"strict schema needs constrained decoding; generator declares "+levelName(caps.ResponseSchema))
		}
		if !caps.ResponseSchema.Sent() {
			req.ResponseSchema = nil
			ignore(domain.FieldResponseSchema)
		}
	}

	strictOpt := func(field string, l domain.Level) error {
		if req.Options.Strict && l != domain.LevelEnforced {
			return domain.UnsupportedField(field, "strict option; generator declares "+levelName(l))
		}
		return nil
	}
	if req.Options.MaxTokens > 0 {
		if err := strictOpt(domain.FieldMaxTokens, caps.MaxTokens); err != nil {
			return req, nil, err
		}
		if !caps.MaxTokens.Sent() {
			req.Options.MaxTokens = 0
			ignore(domain.FieldMaxTokens)
		}
	}
	if req.Options.Temperature != nil {
		if err := strictOpt(domain.FieldTemperature, caps.Sampling); err != nil {
			return req, nil, err
		}
		if !caps.Sampling.Sent() {
			req.Options.Temperature = nil
			ignore(domain.FieldTemperature)
		}
	}
	if len(req.Options.Stop) > 0 {
		if err := strictOpt(domain.FieldStop, caps.Stop); err != nil {
			return req, nil, err
		}
		if !caps.Stop.Sent() {
			req.Options.Stop = nil
			ignore(domain.FieldStop)
		}
	}

	if wantsCache(req) && !caps.CacheBreakpoints.Sent() {
		sys := make([]domain.SystemBlock, len(req.System))
		for i, b := range req.System {
			sys[i] = domain.SystemBlock{Text: b.Text}
		}
		req.System = sys
		ignore(domain.FieldCacheBreakpoints)
		if req.CacheTTL != domain.CacheTTLDefault {
			ignore(domain.FieldCacheTTL)
		}
		req.CacheTTL = domain.CacheTTLDefault
	}
	return req, ignored, nil
}

func levelName(l domain.Level) string {
	if l == "" {
		return string(domain.LevelUnsupported)
	}
	return string(l)
}

// wantsCache reports whether the request opted into cache breakpoints. Caching is
// opt-in through a cache-marked system block: the legacy single-prompt shims set
// none, so their wire requests are unchanged.
func wantsCache(req domain.GenerateRequest) bool {
	for _, b := range req.System {
		if b.Cache {
			return true
		}
	}
	return false
}

// foldSystemIntoMessages prepends the system text to the first user turn (or adds
// one) for a generator with no system role.
func foldSystemIntoMessages(sys []domain.SystemBlock, msgs []domain.ModelMessage) []domain.ModelMessage {
	text := systemText(sys)
	out := make([]domain.ModelMessage, 0, len(msgs)+1)
	folded := false
	for _, m := range msgs {
		if !folded && m.Role == domain.RoleUser {
			m.Content = text + "\n\n" + m.Content
			folded = true
		}
		out = append(out, m)
	}
	if !folded {
		out = append([]domain.ModelMessage{domain.UserMessage(text)}, out...)
	}
	return out
}

// systemText joins system blocks with blank lines — the single-string form for
// providers that take one system instruction.
func systemText(sys []domain.SystemBlock) string {
	parts := make([]string, 0, len(sys))
	for _, b := range sys {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// providerExtensions returns the request's ProviderExtensions addressed to one of
// prefixes, keyed by the bare field name. Keys are "<provider>:<field>" so an
// extension written for one provider never reaches another after a fallback.
func providerExtensions(req domain.GenerateRequest, prefixes ...string) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	for k, v := range req.ProviderExtensions {
		for _, p := range prefixes {
			if field, ok := strings.CutPrefix(k, p+":"); ok && field != "" {
				if out == nil {
					out = make(map[string]json.RawMessage)
				}
				out[field] = v
			}
		}
	}
	return out
}

// schemaMap decodes a JSON Schema for SDKs that take it as a Go value.
func schemaMap(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{"type": "object"}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON schema: %w", err)
	}
	return m, nil
}

func schemaName(rs *domain.ResponseSchema) string {
	if rs.Name != "" {
		return rs.Name
	}
	return "response"
}

// ── Errors ───────────────────────────────────────────────────────────────────

// ProviderError is a provider's HTTP refusal, normalised across the three SDKs so
// the fallback policy can read one status code. The message keeps the
// "<provider>: HTTP <code>: <body>" shape the hand-written clients produced.
type ProviderError struct {
	Provider string
	Status   int
	Message  string
	Err      error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// wrapProviderError turns an SDK error into a ProviderError when it carries an HTTP
// status; anything else (transport failure, cancellation) is returned wrapped with
// the provider name.
func wrapProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return err
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return &ProviderError{Provider: provider, Status: ae.StatusCode, Message: msgOr(ae.RawJSON(), err), Err: err}
	}
	var oe *openai.Error
	if errors.As(err, &oe) {
		return &ProviderError{Provider: provider, Status: oe.StatusCode, Message: msgOr(oe.RawJSON(), err), Err: err}
	}
	var ge genai.APIError
	if errors.As(err, &ge) {
		return &ProviderError{Provider: provider, Status: ge.Code, Message: msgOr(ge.Message, err), Err: err}
	}
	return fmt.Errorf("%s: %w", provider, err)
}

func msgOr(body string, err error) string {
	if b := strings.TrimSpace(body); b != "" {
		return b
	}
	return err.Error()
}

// fallbackClass is what the D1 fallback policy does with a failed call.
type fallbackClass int

const (
	noFallback      fallbackClass = iota // a property of the request or the answer
	fallbackNow                          // the endpoint is unavailable: try another
	refreshThenFall                      // a credential problem: retry once, then fall
)

// classifyForFallback implements ADR-0139 D1: switch provider on 429, 5xx, 401/403
// (after one credential refresh) and on transport failures; never on a refusal (that
// is a successful response with StopRefusal, not an error), a schema failure, an
// unsupported field, a 400-class request error, or the caller's own cancellation.
func classifyForFallback(ctx context.Context, err error) fallbackClass {
	if err == nil || ctx.Err() != nil {
		return noFallback
	}
	if errors.Is(err, domain.ErrSchemaValidation) || errors.Is(err, domain.ErrUnsupported) ||
		errors.Is(err, ErrSpendCeiling) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return noFallback
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		switch {
		case pe.Status == http.StatusUnauthorized || pe.Status == http.StatusForbidden:
			return refreshThenFall
		case pe.Status == http.StatusTooManyRequests || pe.Status >= 500:
			return fallbackNow
		default:
			return noFallback
		}
	}
	// No HTTP status: the request never got an answer (connection refused, reset,
	// DNS). The endpoint is unavailable, the same class as a 503.
	return fallbackNow
}

// ── Shared HTTP client ───────────────────────────────────────────────────────

// sdkHTTPClient is the http.Client every SDK is given. Retries live in ONE place,
// sharedLLMTransport (429/5xx backoff with jitter, Retry-After, the quota and
// context-overflow sniffs); every SDK's own retry loop is disabled so a 429 is not
// retried 5×3 times. Streaming calls get no total timeout — http.Client.Timeout caps
// body reads too and would kill a slow model mid-stream; ctx governs them.
func sdkHTTPClient(timeoutMs int, streaming bool) *http.Client {
	hc := &http.Client{Transport: &contentTypeTolerance{base: sharedLLMTransport, streaming: streaming}}
	if !streaming && timeoutMs > 0 {
		hc.Timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	return hc
}

// contentTypeTolerance restores the hand-written clients' tolerance of a missing or
// wrong Content-Type on a SUCCESSFUL response. The SDKs refuse to decode a 2xx body
// that is not labelled application/json (or text/event-stream for a stream), and
// OpenAI-compatible proxies and gateways do not all label theirs; the old clients
// decoded the body regardless. Error responses are left untouched.
type contentTypeTolerance struct {
	base      http.RoundTripper
	streaming bool
}

func (t *contentTypeTolerance) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, err
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case t.streaming && !strings.Contains(ct, "text/event-stream"):
		resp.Header.Set("Content-Type", "text/event-stream")
	case !t.streaming && !strings.Contains(ct, "json"):
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp, nil
}

// ── Usage ────────────────────────────────────────────────────────────────────

// tokenUsageOf converts C1 usage to the spend ledger's TokenUsage.
func tokenUsageOf(u domain.GenerateUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.Input,
		CompletionTokens: u.Output,
		TotalTokens:      u.Input + u.Output,
		CacheReadTokens:  u.CacheRead,
		Estimated:        u.Estimated,
	}
}

// usageChunk stamps a final StreamChunk with the call's usage, for the legacy
// consumers that read only the Usage* fields.
func usageChunk(resp *domain.GenerateResponse) domain.StreamChunk {
	u := resp.Usage
	return domain.StreamChunk{
		IsFinal:               true,
		UsagePromptTokens:     u.Input,
		UsageCompletionTokens: u.Output,
		UsageTotalTokens:      u.Input + u.Output,
		UsageCacheReadTokens:  u.CacheRead,
		Response:              resp,
	}
}

// RequestText flattens a request for the len/4 admission estimate (the Provider
// meter and the agent-plane gateway, which admits lease calls itself).
func RequestText(req domain.GenerateRequest) string {
	var b strings.Builder
	b.WriteString(systemText(req.System))
	b.WriteString(MessagesText(req.Messages))
	for _, t := range req.Tools {
		b.WriteString(t.Name)
		b.WriteString(t.Description)
		b.Write(t.Parameters)
	}
	if req.ResponseSchema != nil {
		b.Write(req.ResponseSchema.Schema)
	}
	return b.String()
}

// ── Legacy-port shims ────────────────────────────────────────────────────────
//
// The pre-C1 ports are thin shims over GenerateRequest, so an adapter has ONE
// request builder and the 24 existing callers keep their behaviour.

// shimGenerate is Generator.Generate: one user turn, the text back.
func shimGenerate(ctx context.Context, rg domain.RequestGenerator, prompt string) (string, error) {
	resp, err := rg.GenerateRequest(ctx, domain.UserPrompt(prompt))
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// shimGenerateWithTools is ToolCallingGenerator.GenerateWithTools.
func shimGenerateWithTools(ctx context.Context, rg domain.RequestGenerator, messages []domain.ModelMessage, tools []domain.ToolDefinition) (domain.ModelTurn, error) {
	resp, err := rg.GenerateRequest(ctx, domain.GenerateRequest{Messages: messages, Tools: tools})
	if err != nil {
		return domain.ModelTurn{}, err
	}
	return resp.Turn(), nil
}

// shimGenerateStream is LLMStreamer.GenerateStream.
func shimGenerateStream(ctx context.Context, rg domain.RequestGenerator, prompt string) (<-chan domain.StreamChunk, error) {
	return rg.StreamRequest(ctx, domain.UserPrompt(prompt))
}

// streamFinal sends the closing chunk of a stream: the stream_error convention the
// hand-written streamers used when the stream broke, else the usage-and-response
// chunk.
func streamFinal(ch chan<- domain.StreamChunk, resp *domain.GenerateResponse, err error) {
	if err != nil {
		ch <- domain.StreamChunk{IsFinal: true, Text: fmt.Sprintf("stream_error: %v", err)}
		return
	}
	ch <- usageChunk(resp)
}
