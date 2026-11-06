package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// AnthropicClient is the Anthropic Messages adapter (ADR-0139 D1), built on the
// official anthropic-sdk-go.
//
// Prompt caching follows the layout ADR-0139 D1 adopted from Hermes: a breakpoint
// on the LAST cache-marked system block (everything before it is the stable,
// cached prefix), plus rolling breakpoints on the last up to three messages, so a
// multi-turn conversation re-reads its own history from cache. Anthropic allows at
// most four breakpoints per request; the layout never exceeds that. Caching is
// opt-in: a request with no cache-marked system block gets no breakpoint at all,
// so the legacy single-prompt shims send exactly what they sent before.
type AnthropicClient struct {
	// Endpoint is the API root. The conventional ".../v1" is accepted: the SDK adds
	// the version segment itself.
	Endpoint  string
	Model     string
	APIKeyEnv string
	// GeneratorID names this generator in the credential store. Empty for a
	// client built straight from config with no store entry of its own -- the
	// environment path still applies.
	GeneratorID string
	TimeoutMs   int
	// MaxTokens is the output cap sent when a request sets none. The Messages API
	// requires one; 0 falls back to config.AnthropicDefaultMaxTokens, the same value
	// config.ApplyGeneratorDefaults fills (this replaces the hardcoded 4096).
	MaxTokens int
	// NativeTools gates the tool-use path exactly as it does for OpenAIClient
	// (ADR-0097 D2). The Messages API supports tools on every current model, but the
	// hand-written client had no tool path, so the gateway sent Anthropic agents down
	// the text protocol; turning it on silently would change every such deployment's
	// agent loop. It is a declared config switch (native_tools: true), per Q4.
	NativeTools bool
	// Caps overrides the declared capabilities when non-nil.
	Caps *domain.Capabilities
}

// maxCacheBreakpoints is Anthropic's per-request limit on cache_control marks.
const maxCacheBreakpoints = 4

// rollingMessageBreakpoints is the Hermes layout's message-tail depth.
const rollingMessageBreakpoints = 3

// NativeToolsEnabled implements domain.ToolCallingReporter.
func (c *AnthropicClient) NativeToolsEnabled() bool { return c.Capabilities().Tools.Sent() }

// RequestsEnabled implements domain.RequestGeneratorReporter.
func (c *AnthropicClient) RequestsEnabled() bool { return true }

// Capabilities implements domain.RequestGenerator. Structured outputs
// (output_config.format) are constrained decoding on the models that support them;
// a generator serving an older model should lower response_schema in config.
func (c *AnthropicClient) Capabilities() domain.Capabilities {
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
		ResponseSchema:   domain.LevelEnforced,
		MaxTokens:        domain.LevelEnforced,
		Sampling:         domain.LevelEnforced,
		Stop:             domain.LevelEnforced,
		CacheBreakpoints: domain.LevelEnforced,
		Images:           domain.LevelUnsupported, // no image content in C1 messages yet
	}
}

func (c *AnthropicClient) client(streaming bool) anthropic.Client {
	opts := []option.RequestOption{
		// Credentials are resolved by APIKeyFor (store, then env), never by the
		// SDK's own autoloader (ANTHROPIC_API_KEY, profiles, federation).
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(sdkHTTPClient(c.TimeoutMs, streaming)),
		// Retries are sharedLLMTransport's job (see sdkHTTPClient).
		option.WithMaxRetries(0),
	}
	if base := anthropicBaseURL(c.Endpoint); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if key := APIKeyFor(c.GeneratorID, c.APIKeyEnv); key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	return anthropic.NewClient(opts...)
}

// anthropicBaseURL turns the configured endpoint (conventionally ".../v1", which
// the hand-written client appended "/messages" to) into the SDK's base URL, which
// must NOT carry the version segment.
func anthropicBaseURL(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	if e == "" {
		return ""
	}
	e = strings.TrimSuffix(e, "/v1")
	return e + "/"
}

func (c *AnthropicClient) cacheControl(ttl domain.CacheTTL) anthropic.CacheControlEphemeralParam {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch ttl {
	case domain.CacheTTLShort:
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL5m
	case domain.CacheTTLLong:
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL1h
	}
	return cc
}

func (c *AnthropicClient) params(req domain.GenerateRequest) (anthropic.MessageNewParams, []option.RequestOption, error) {
	maxTokens := req.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = config.AnthropicDefaultMaxTokens
	}
	p := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.Model),
		MaxTokens: int64(maxTokens),
	}
	caching := wantsCache(req)
	cc := c.cacheControl(req.CacheTTL)

	breakpoints := 0
	lastCached := -1
	for i, b := range req.System {
		if b.Cache {
			lastCached = i
		}
	}
	for i, b := range req.System {
		if b.Text == "" {
			continue
		}
		blk := anthropic.TextBlockParam{Text: b.Text}
		if caching && i == lastCached {
			blk.CacheControl = cc
			breakpoints++
		}
		p.System = append(p.System, blk)
	}

	msgs, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return p, nil, err
	}
	if caching {
		// Rolling breakpoints on the last up-to-3 messages, within the 4-mark cap.
		for i := len(msgs) - 1; i >= 0 && i >= len(msgs)-rollingMessageBreakpoints && breakpoints < maxCacheBreakpoints; i-- {
			blocks := msgs[i].Content
			if len(blocks) == 0 {
				continue
			}
			if mark := blocks[len(blocks)-1].GetCacheControl(); mark != nil {
				*mark = cc
				breakpoints++
			}
		}
	}
	p.Messages = msgs

	for _, t := range req.Tools {
		schema, err := anthropicInputSchema(t.Parameters)
		if err != nil {
			return p, nil, fmt.Errorf("anthropic: tool %q: %w", t.Name, err)
		}
		tp := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = anthropic.String(t.Description)
		}
		p.Tools = append(p.Tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	if len(p.Tools) > 0 {
		var disable = anthropic.Bool(req.ToolChoice.DisableParallel)
		switch req.ToolChoice.Mode {
		case domain.ToolChoiceNone:
			p.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		case domain.ToolChoiceRequired:
			p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
			if req.ToolChoice.DisableParallel {
				p.ToolChoice.OfAny.DisableParallelToolUse = disable
			}
		case domain.ToolChoiceTool:
			p.ToolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: req.ToolChoice.Name}}
			if req.ToolChoice.DisableParallel {
				p.ToolChoice.OfTool.DisableParallelToolUse = disable
			}
		default:
			if req.ToolChoice.DisableParallel {
				p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: disable}}
			}
		}
	}
	if rs := req.ResponseSchema; rs != nil {
		schema, err := schemaMap(rs.Schema)
		if err != nil {
			return p, nil, fmt.Errorf("anthropic: response schema: %w", err)
		}
		p.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: schema}
	}
	if t := req.Options.Temperature; t != nil {
		p.Temperature = anthropic.Float(float64(*t))
	}
	if len(req.Options.Stop) > 0 {
		p.StopSequences = req.Options.Stop
	}

	var opts []option.RequestOption
	for k, v := range providerExtensions(req, "anthropic") {
		opts = append(opts, option.WithJSONSet(k, json.RawMessage(v)))
	}
	return p, opts, nil
}

// anthropicInputSchema carries a raw JSON Schema object through the SDK's typed
// input_schema: properties and required map to their fields, every other keyword
// ($defs, additionalProperties, ...) rides in ExtraFields unchanged.
func anthropicInputSchema(raw []byte) (anthropic.ToolInputSchemaParam, error) {
	m, err := schemaMap(raw)
	if err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}
	var s anthropic.ToolInputSchemaParam
	if props, ok := m["properties"]; ok {
		s.Properties = props
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				s.Required = append(s.Required, name)
			}
		}
	}
	extra := map[string]any{}
	for k, v := range m {
		switch k {
		case "properties", "required", "type":
		default:
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		s.ExtraFields = extra
	}
	return s, nil
}

// toAnthropicMessages renders the conversation. Anthropic requires every
// tool_result answering one assistant turn in the NEXT single user message, so
// consecutive tool turns (and a user turn that follows them) are merged.
func toAnthropicMessages(messages []domain.ModelMessage) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	appendUser := func(blocks ...anthropic.ContentBlockParamUnion) {
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleUser {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.NewUserMessage(blocks...))
	}
	for _, m := range messages {
		switch m.Role {
		case domain.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Arguments)
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				if !json.Valid(args) {
					return nil, fmt.Errorf("anthropic: tool call %q has invalid JSON arguments", tc.ID)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, args, tc.Name))
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock(""))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case domain.RoleTool:
			appendUser(anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false))
		default:
			appendUser(anthropic.NewTextBlock(m.Content))
		}
	}
	return out, nil
}

// GenerateRequest implements domain.RequestGenerator.
func (c *AnthropicClient) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	p, opts, err := c.params(req)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	cl := c.client(false)
	msg, err := cl.Messages.New(ctx, p, opts...)
	if err != nil {
		return domain.GenerateResponse{}, wrapProviderError("anthropic", err)
	}
	resp := anthropicResponse(msg)
	resp.Ignored = ignored
	// ADR-0131 D2: hand the response's own usage to whoever metered this call.
	reportUsage(ctx, tokenUsageOf(resp.Usage))
	if len(msg.Content) == 0 && resp.StopReason != domain.StopRefusal {
		return domain.GenerateResponse{}, fmt.Errorf("anthropic: no content in response")
	}
	return resp, nil
}

func anthropicResponse(msg *anthropic.Message) domain.GenerateResponse {
	var text strings.Builder
	out := domain.GenerateResponse{
		StopReason: normalizeAnthropicStopReason(msg.StopReason),
		Usage:      anthropicUsageOf(msg.Usage),
		Model:      string(msg.Model),
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := []byte(b.Input)
			if len(args) == 0 {
				args = []byte(`{}`)
			}
			out.ToolCalls = append(out.ToolCalls, domain.ModelToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	out.Text = text.String()
	return out
}

// anthropicUsageOf maps Anthropic usage. input_tokens EXCLUDES cache reads and
// writes, so the C1 Input (every input token) is the sum of all three; Anthropic
// reports writes, so CacheWriteKnown is true.
func anthropicUsageOf(u anthropic.Usage) domain.GenerateUsage {
	return domain.GenerateUsage{
		Input:           int(u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens),
		Output:          int(u.OutputTokens),
		CacheRead:       int(u.CacheReadInputTokens),
		CacheWrite:      int(u.CacheCreationInputTokens),
		CacheWriteKnown: true,
	}
}

func normalizeAnthropicStopReason(r anthropic.StopReason) domain.StopReason {
	switch r {
	case anthropic.StopReasonEndTurn:
		return domain.StopEndTurn
	case anthropic.StopReasonToolUse:
		return domain.StopToolUse
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		return domain.StopMaxTokens
	case anthropic.StopReasonStopSequence:
		return domain.StopSequence
	case anthropic.StopReasonRefusal:
		return domain.StopRefusal
	default:
		// pause_turn and anything new: not finished (see domain.StopUnknown).
		return domain.StopUnknown
	}
}

// StreamRequest implements domain.RequestGenerator.
func (c *AnthropicClient) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return nil, err
	}
	p, opts, err := c.params(req)
	if err != nil {
		return nil, err
	}
	cl := c.client(true)
	stream := cl.Messages.NewStreaming(ctx, p, opts...)
	// Pull the first event so an HTTP refusal is returned synchronously.
	first := stream.Next()
	if !first {
		if serr := stream.Err(); serr != nil {
			_ = stream.Close()
			return nil, wrapProviderError("anthropic", serr)
		}
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		defer stream.Close()
		var msg anthropic.Message
		for ok := first; ok; ok = stream.Next() {
			if ctx.Err() != nil {
				return
			}
			ev := stream.Current()
			if err := msg.Accumulate(ev); err != nil {
				streamFinal(out, nil, fmt.Errorf("anthropic: %w", err))
				return
			}
			if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				out <- domain.StreamChunk{Text: ev.Delta.Text}
			}
			// ADR-0139 amendment 2026-11-06: tool-call fragments, so the gateway can
			// forward a final-answer argument live. The call opens on its
			// content_block_start (id, name) and its input arrives as input_json_delta
			// pieces on the same block index. msg.Accumulate above still builds the
			// complete calls for the final Response.
			if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" {
				out <- domain.StreamChunk{ToolCall: &domain.ToolCallDelta{
					Index: int(ev.Index), ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}}
			}
			if ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "" {
				out <- domain.StreamChunk{ToolCall: &domain.ToolCallDelta{
					Index: int(ev.Index), ArgumentsDelta: ev.Delta.PartialJSON}}
			}
		}
		if ctx.Err() != nil {
			return
		}
		resp := anthropicResponse(&msg)
		resp.Ignored = ignored
		reportUsage(ctx, tokenUsageOf(resp.Usage))
		streamFinal(out, &resp, wrapStreamErr("anthropic", stream.Err()))
	}()
	return out, nil
}

// Generate implements domain.Generator.
func (c *AnthropicClient) Generate(ctx context.Context, prompt string) (string, error) {
	return shimGenerate(ctx, c, prompt)
}

// GenerateWithTools implements domain.ToolCallingGenerator. New in C1: the
// hand-written Anthropic client had no native tool path at all.
func (c *AnthropicClient) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	if !c.NativeToolsEnabled() {
		return domain.ModelTurn{}, fmt.Errorf("anthropic: native tool-calling not enabled for model %q (set native_tools)", c.Model)
	}
	return shimGenerateWithTools(ctx, c, messages, tools)
}

// GenerateStream implements domain.LLMStreamer.
func (c *AnthropicClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	return shimGenerateStream(ctx, c, prompt)
}

var (
	_ domain.RequestGenerator     = (*AnthropicClient)(nil)
	_ domain.ToolCallingGenerator = (*AnthropicClient)(nil)
	_ domain.LLMStreamer          = (*AnthropicClient)(nil)
)
