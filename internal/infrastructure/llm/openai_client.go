package llm

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/cambrian-sh/core/domain"
)

// OpenAIClient is the OpenAI-COMPATIBLE adapter (ADR-0139 D1), built on the
// official openai-go SDK. "Compatible" is the operative word: it also serves the
// claudebox proxy behind haiku-local, Ollama's /v1 endpoint (OllamaClient),
// deepseek and mimo on opencode, and any LiteLLM a user points it at (owner Q2).
// Those endpoints differ in what they honour, so the capability declaration is
// per generator (Caps, from GeneratorConfig.RequestCapabilities), not per adapter.
type OpenAIClient struct {
	Endpoint  string
	Model     string
	APIKeyEnv string
	// GeneratorID names this generator in the credential store. Empty for a
	// client built straight from config with no store entry of its own -- the
	// environment path still applies.
	GeneratorID string
	TimeoutMs   int
	// DisableThinking sends thinking:{"type":"disabled"} to suppress server-side
	// reasoning on OpenAI-compat reasoning models (deepseek-v4-flash on opencode).
	// A request's "openai:thinking" ProviderExtension overrides it per call.
	DisableThinking bool
	// NativeTools declares that this endpoint honours the tool-calling API
	// (ADR-0097 D2). Default false: sending `tools` to an endpoint that ignores them
	// yields a model that never calls anything, which reads as a model-quality
	// problem rather than a configuration one.
	NativeTools bool
	// MaxTokens is the output cap sent when a request sets none; 0 sends none.
	MaxTokens int
	// Caps overrides DefaultCapabilities when non-nil (the per-generator config).
	Caps *domain.Capabilities

	// provider names the adapter in errors and extension keys ("openai", or
	// "ollama" when wrapped by OllamaClient). Empty = "openai".
	provider string
}

// NativeToolsEnabled implements domain.ToolCallingReporter.
func (c *OpenAIClient) NativeToolsEnabled() bool { return c.Capabilities().Tools.Sent() }

// RequestsEnabled implements domain.RequestGeneratorReporter.
func (c *OpenAIClient) RequestsEnabled() bool { return true }

func (c *OpenAIClient) name() string {
	if c.provider == "" {
		return "openai"
	}
	return c.provider
}

// Capabilities implements domain.RequestGenerator.
//
// The defaults are what an unknown OpenAI-compatible endpoint can be trusted with:
// json_schema is best-effort (deepseek accepts only json_object; claudebox pastes
// the schema into the prompt), so the provider layer validates and repairs it;
// caching is implicit on every such endpoint, never an explicit breakpoint. A real
// OpenAI generator can raise response_schema to "enforced" in its config.
func (c *OpenAIClient) Capabilities() domain.Capabilities {
	if c.Caps != nil {
		return *c.Caps
	}
	tools := domain.LevelUnsupported
	if c.NativeTools {
		tools = domain.LevelEnforced
	}
	return domain.Capabilities{
		SystemRole:       domain.LevelEnforced,
		Tools:            tools,
		ToolChoice:       domain.LevelEnforced,
		ParallelTools:    domain.LevelEnforced,
		ResponseSchema:   domain.LevelBestEffort,
		MaxTokens:        domain.LevelEnforced,
		Sampling:         domain.LevelEnforced,
		Stop:             domain.LevelEnforced,
		CacheBreakpoints: domain.LevelUnsupported,
		Images:           domain.LevelUnsupported,
	}
}

// client builds the SDK client for one call. Built per call, not cached, so a
// credential rotated in the store (ADR-0101 D5) applies on the next call — which is
// what makes the D1 "retry once after a credential refresh" meaningful.
func (c *OpenAIClient) client(streaming bool) openai.Client {
	key := APIKeyFor(c.GeneratorID, c.APIKeyEnv)
	opts := []option.RequestOption{
		option.WithBaseURL(strings.TrimRight(c.Endpoint, "/") + "/"),
		option.WithHTTPClient(sdkHTTPClient(c.TimeoutMs, streaming)),
		// Retries are sharedLLMTransport's job (see sdkHTTPClient).
		option.WithMaxRetries(0),
		// The SDK seeds credentials, org and project from OPENAI_* env vars. This
		// adapter talks to many endpoints that are not OpenAI; an ambient
		// OPENAI_API_KEY must never be sent to a local Ollama or a third-party
		// gateway. The credential is ours (APIKeyFor) or absent.
		option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			r.Header.Del("OpenAI-Organization")
			r.Header.Del("OpenAI-Project")
			if key == "" {
				r.Header.Del("Authorization")
			}
			return next(r)
		}),
	}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	} else {
		// A non-empty placeholder keeps the SDK from failing on a missing key; the
		// middleware strips the header it produces.
		opts = append(opts, option.WithAPIKey("unused"))
	}
	return openai.NewClient(opts...)
}

// params maps a prepared request onto the SDK's chat-completions params.
func (c *OpenAIClient) params(req domain.GenerateRequest) (openai.ChatCompletionNewParams, []option.RequestOption, error) {
	p := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(c.Model),
		Messages: toOpenAIMessages(req.System, req.Messages),
	}
	for _, t := range req.Tools {
		params, err := schemaMap(t.Parameters)
		if err != nil {
			return p, nil, fmt.Errorf("%s: tool %q: %w", c.name(), t.Name, err)
		}
		def := shared.FunctionDefinitionParam{Name: t.Name, Parameters: shared.FunctionParameters(params)}
		if t.Description != "" {
			def.Description = openai.String(t.Description)
		}
		p.Tools = append(p.Tools, openai.ChatCompletionFunctionTool(def))
	}
	// Only constrain the choice when tools are actually offered; sending
	// tool_choice with an empty tools array is rejected by some gateways.
	if len(p.Tools) > 0 {
		switch req.ToolChoice.Mode {
		case domain.ToolChoiceNone:
			p.ToolChoice.OfAuto = openai.String("none")
		case domain.ToolChoiceRequired:
			p.ToolChoice.OfAuto = openai.String("required")
		case domain.ToolChoiceTool:
			p.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
				openai.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ToolChoice.Name})
		default:
			p.ToolChoice.OfAuto = openai.String("auto")
		}
		if req.ToolChoice.DisableParallel {
			p.ParallelToolCalls = openai.Bool(false)
		}
	}
	if rs := req.ResponseSchema; rs != nil {
		schema, err := schemaMap(rs.Schema)
		if err != nil {
			return p, nil, fmt.Errorf("%s: response schema: %w", c.name(), err)
		}
		js := shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: schemaName(rs), Schema: schema}
		if rs.Strict {
			js.Strict = openai.Bool(true)
		}
		p.ResponseFormat.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{JSONSchema: js}
	}
	// max_tokens, not max_completion_tokens: every compatible endpoint we serve
	// (opencode, claudebox, Ollama, LiteLLM) reads max_tokens; only OpenAI's o-series
	// insists on the newer name.
	if n := req.Options.MaxTokens; n > 0 {
		p.MaxTokens = openai.Int(int64(n))
	} else if c.MaxTokens > 0 {
		p.MaxTokens = openai.Int(int64(c.MaxTokens))
	}
	if t := req.Options.Temperature; t != nil {
		p.Temperature = openai.Float(float64(*t))
	}
	if len(req.Options.Stop) > 0 {
		p.Stop.OfStringArray = req.Options.Stop
	}

	// Extensions go on the body as top-level fields. The legacy disable_thinking
	// flag is the default for "thinking"; a request extension overrides it.
	ext := providerExtensions(req, "openai", c.name())
	var opts []option.RequestOption
	if _, set := ext["thinking"]; !set && c.DisableThinking {
		opts = append(opts, option.WithJSONSet("thinking", map[string]string{"type": "disabled"}))
	}
	keys := make([]string, 0, len(ext))
	for k := range ext {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		opts = append(opts, option.WithJSONSet(k, ext[k]))
	}
	return p, opts, nil
}

// GenerateRequest implements domain.RequestGenerator.
func (c *OpenAIClient) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	p, opts, err := c.params(req)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	cl := c.client(false)
	completion, err := cl.Chat.Completions.New(ctx, p, opts...)
	if err != nil {
		c.logRejected(err, p)
		return domain.GenerateResponse{}, wrapProviderError(c.name(), err)
	}
	usage := openAIUsage(completion.Usage)
	// ADR-0131 D2: hand the response's own usage to whoever metered this call.
	reportUsage(ctx, tokenUsageOf(usage))
	if len(completion.Choices) == 0 {
		return domain.GenerateResponse{}, fmt.Errorf("%s: no choices in response", c.name())
	}
	ch := completion.Choices[0]
	out := domain.GenerateResponse{
		Text:       ch.Message.Content,
		StopReason: normalizeOpenAIFinishReason(ch.FinishReason),
		Usage:      usage,
		Ignored:    ignored,
		Model:      completion.Model,
	}
	if out.Text == "" && ch.Message.Refusal != "" {
		out.Text, out.StopReason = ch.Message.Refusal, domain.StopRefusal
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, domain.ModelToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			// Arguments arrive as a JSON string; forward the bytes unparsed.
			// Schema validation belongs to whoever owns the tool.
			Arguments: []byte(tc.Function.Arguments),
		})
	}
	return out, nil
}

// logRejected records what a tools request OFFERED when the endpoint refused it.
// A gateway rejection says only "Upstream request failed" and names no field; tool
// names and schemas are what gets rejected in practice (a name containing ':' or
// '/', a schema missing its top-level "type": "object"). The prompt is never logged.
func (c *OpenAIClient) logRejected(err error, p openai.ChatCompletionNewParams) {
	if len(p.Tools) == 0 {
		return
	}
	names := make([]string, 0, len(p.Tools))
	for _, t := range p.Tools {
		if f := t.GetFunction(); f != nil {
			names = append(names, f.Name)
		}
	}
	slog.Error(c.name()+": request rejected", "model", c.Model,
		"tool_count", len(p.Tools), "tool_names", names, "err", err)
}

// StreamRequest implements domain.RequestGenerator. Usage is requested with
// stream_options.include_usage; the final chunk carries the whole response.
func (c *OpenAIClient) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return nil, err
	}
	p, opts, err := c.params(req)
	if err != nil {
		return nil, err
	}
	p.StreamOptions.IncludeUsage = openai.Bool(true)
	cl := c.client(true)
	stream := cl.Chat.Completions.NewStreaming(ctx, p, opts...)
	// The SDK sends the request lazily, on the first Next. Pull it here so an HTTP
	// refusal is returned synchronously, as the hand-written streamer did.
	first := stream.Next()
	if !first {
		if serr := stream.Err(); serr != nil {
			_ = stream.Close()
			return nil, wrapProviderError(c.name(), serr)
		}
	}

	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		defer stream.Close()
		acc := openAIStreamAcc{resp: domain.GenerateResponse{Ignored: ignored}}
		for ok := first; ok; ok = stream.Next() {
			if ctx.Err() != nil {
				return
			}
			chunk := stream.Current()
			text, calls := acc.addDeltas(chunk)
			if text != "" {
				out <- domain.StreamChunk{Text: text}
			}
			// ADR-0139 amendment 2026-11-06: tool-call fragments for the live
			// step-output stream (the accumulator still builds the whole calls).
			for i := range calls {
				out <- domain.StreamChunk{ToolCall: &calls[i]}
			}
		}
		if ctx.Err() != nil {
			return
		}
		resp := acc.finish()
		reportUsage(ctx, tokenUsageOf(resp.Usage))
		streamFinal(out, &resp, wrapStreamErr(c.name(), stream.Err()))
	}()
	return out, nil
}

func wrapStreamErr(provider string, err error) error {
	if err == nil {
		return nil
	}
	return wrapProviderError(provider, err)
}

// openAIStreamAcc folds stream deltas into one response. Tool-call deltas arrive
// fragmented by index: the id and name on the first, arguments across many.
type openAIStreamAcc struct {
	resp  domain.GenerateResponse
	text  strings.Builder
	calls []domain.ModelToolCall
	byIdx map[int64]int
}

// addDeltas folds one chunk and returns its text delta and its tool-call
// fragments, indexed by the call's position in this turn.
func (a *openAIStreamAcc) addDeltas(chunk openai.ChatCompletionChunk) (string, []domain.ToolCallDelta) {
	if chunk.Model != "" {
		a.resp.Model = chunk.Model
	}
	if u := openAIUsage(chunk.Usage); u.Input+u.Output > 0 {
		a.resp.Usage = u
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}
	ch := chunk.Choices[0]
	var deltas []domain.ToolCallDelta
	if ch.FinishReason != "" {
		a.resp.StopReason = normalizeOpenAIFinishReason(ch.FinishReason)
	}
	for _, d := range ch.Delta.ToolCalls {
		if a.byIdx == nil {
			a.byIdx = map[int64]int{}
		}
		i, seen := a.byIdx[d.Index]
		if !seen {
			i = len(a.calls)
			a.byIdx[d.Index] = i
			a.calls = append(a.calls, domain.ModelToolCall{})
		}
		if d.ID != "" {
			a.calls[i].ID = d.ID
		}
		if d.Function.Name != "" {
			a.calls[i].Name = d.Function.Name
		}
		a.calls[i].Arguments = append(a.calls[i].Arguments, d.Function.Arguments...)
		deltas = append(deltas, domain.ToolCallDelta{
			Index: i, ID: d.ID, Name: d.Function.Name, ArgumentsDelta: d.Function.Arguments})
	}
	a.text.WriteString(ch.Delta.Content)
	return ch.Delta.Content, deltas
}

func (a *openAIStreamAcc) finish() domain.GenerateResponse {
	a.resp.Text = a.text.String()
	a.resp.ToolCalls = a.calls
	if a.resp.StopReason == "" {
		a.resp.StopReason = domain.StopUnknown
	}
	return a.resp
}

// openAIUsage maps the SDK usage block. prompt_tokens already includes the cached
// tokens, so CacheRead is a breakdown. OpenAI-compatible endpoints report no cache
// WRITES, so CacheWriteKnown stays false.
func openAIUsage(u openai.CompletionUsage) domain.GenerateUsage {
	return domain.GenerateUsage{
		Input:     int(u.PromptTokens),
		Output:    int(u.CompletionTokens),
		CacheRead: int(u.PromptTokensDetails.CachedTokens),
	}
}

// Generate implements domain.Generator (the legacy single-prompt port).
func (c *OpenAIClient) Generate(ctx context.Context, prompt string) (string, error) {
	return shimGenerate(ctx, c, prompt)
}

// GenerateWithTools implements domain.ToolCallingGenerator (ADR-0097).
//
// Refuses rather than quietly falling back to a plain completion when tools are
// not declared: a caller that reached here without checking SupportsToolCalling
// has a bug, and silently dropping the tools would hand it a model that never
// calls anything.
func (c *OpenAIClient) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	if !c.NativeToolsEnabled() {
		return domain.ModelTurn{}, fmt.Errorf("%s: native tool-calling not enabled for model %q (set native_tools)", c.name(), c.Model)
	}
	return shimGenerateWithTools(ctx, c, messages, tools)
}

// GenerateStream implements domain.LLMStreamer.
func (c *OpenAIClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	return shimGenerateStream(ctx, c, prompt)
}

// normalizeOpenAIFinishReason maps OpenAI's vocabulary onto domain.StopReason.
//
// Anything unrecognised becomes StopUnknown, which is NOT finished — a provider we
// have not seen before must degrade to "keep going", never to "done".
func normalizeOpenAIFinishReason(r string) domain.StopReason {
	switch r {
	case "tool_calls", "function_call":
		return domain.StopToolUse
	case "stop":
		return domain.StopEndTurn
	case "length":
		return domain.StopMaxTokens
	case "content_filter":
		return domain.StopRefusal
	default:
		return domain.StopUnknown
	}
}

// toOpenAIMessages renders the system blocks and the conversation onto the wire.
//
// System blocks become ONE system message: OpenAI-compatible endpoints differ on
// multiple system messages (claudebox merges them anyway), and one message keeps the
// stable prefix byte-identical across calls, which is what implicit caching keys on.
// An assistant turn carries its tool_calls; a tool turn carries the id of the call it
// answers. Dropping either leaves the model unable to see its own call or its result.
func toOpenAIMessages(system []domain.SystemBlock, messages []domain.ModelMessage) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if s := systemText(system); s != "" {
		out = append(out, openai.SystemMessage(s))
	}
	for _, m := range messages {
		switch m.Role {
		case domain.RoleAssistant:
			a := openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" || len(m.ToolCalls) == 0 {
				a.Content.OfString = openai.String(m.Content)
			}
			for _, tc := range m.ToolCalls {
				// Arguments go back as the JSON STRING the provider sent, not
				// re-encoded: round-tripping through a map would reorder keys and
				// change the bytes the provider matches against its record of the call.
				args := string(tc.Arguments)
				if args == "" {
					args = "{}"
				}
				a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: tc.Name, Arguments: args,
						},
					},
				})
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &a})
		case domain.RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		case "system":
			out = append(out, openai.SystemMessage(m.Content))
		default:
			out = append(out, openai.UserMessage(m.Content))
		}
	}
	return out
}

var (
	_ domain.RequestGenerator     = (*OpenAIClient)(nil)
	_ domain.ToolCallingGenerator = (*OpenAIClient)(nil)
	_ domain.LLMStreamer          = (*OpenAIClient)(nil)
)
