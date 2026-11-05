package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"google.golang.org/genai"

	"github.com/cambrian-sh/core/domain"
)

// GeminiClient is the Google Gemini adapter (ADR-0139 D1), built on the official
// google.golang.org/genai SDK against the Gemini API backend.
//
// systemInstruction, functionDeclarations (by raw JSON Schema), responseJsonSchema
// (constrained decoding — enforced), maxOutputTokens, temperature and
// stopSequences are all native. Gemini caches implicitly or through a separate
// cachedContent resource, never through per-request breakpoints, so
// cache_breakpoints is declared unsupported and reported as ignored.
type GeminiClient struct {
	Endpoint  string // default: https://generativelanguage.googleapis.com/v1beta
	Model     string // e.g. "gemini-2.0-flash"; "models/" prefix tolerated
	APIKeyEnv string // env var name holding the API key (default: GEMINI_API_KEY)
	// GeneratorID names this generator in the credential store. Empty for a
	// client built straight from config with no store entry of its own -- the
	// environment path still applies.
	GeneratorID string
	TimeoutMs   int
	// MaxTokens is the output cap sent when a request sets none; 0 sends none.
	MaxTokens int
	// NativeTools gates function calling, as for the other adapters (ADR-0097 D2).
	NativeTools bool
	// Caps overrides the declared capabilities when non-nil.
	Caps *domain.Capabilities
}

// NativeToolsEnabled implements domain.ToolCallingReporter.
func (c *GeminiClient) NativeToolsEnabled() bool { return c.Capabilities().Tools.Sent() }

// RequestsEnabled implements domain.RequestGeneratorReporter.
func (c *GeminiClient) RequestsEnabled() bool { return true }

// Capabilities implements domain.RequestGenerator.
func (c *GeminiClient) Capabilities() domain.Capabilities {
	if c.Caps != nil {
		return *c.Caps
	}
	tools := domain.LevelUnsupported
	if c.NativeTools {
		tools = domain.LevelEnforced
	}
	return domain.Capabilities{
		SystemRole:     domain.LevelEnforced,
		Tools:          tools,
		ToolChoice:     domain.LevelEnforced,
		ParallelTools:  domain.LevelUnsupported, // no "at most one call" switch
		ResponseSchema: domain.LevelEnforced,
		MaxTokens:      domain.LevelEnforced,
		Sampling:       domain.LevelEnforced,
		Stop:           domain.LevelEnforced,
		// Implicit caching only; explicit caching is a separate resource.
		CacheBreakpoints: domain.LevelUnsupported,
		Images:           domain.LevelUnsupported,
	}
}

func (c *GeminiClient) apiKey() string {
	env := c.APIKeyEnv
	if env == "" {
		env = "GEMINI_API_KEY"
	}
	return APIKeyFor(c.GeneratorID, env)
}

// geminiEndpoint splits the configured endpoint (".../v1beta", as the hand-written
// client took it) into the SDK's base URL and API version.
func geminiEndpoint(endpoint string) (base, version string) {
	e := strings.TrimRight(endpoint, "/")
	if e == "" {
		return "", ""
	}
	for _, v := range []string{"v1beta", "v1alpha", "v1"} {
		if strings.HasSuffix(e, "/"+v) {
			return strings.TrimSuffix(e, v), v
		}
	}
	return e + "/", ""
}

func (c *GeminiClient) client(ctx context.Context, streaming bool) (*genai.Client, error) {
	base, version := geminiEndpoint(c.Endpoint)
	cc := &genai.ClientConfig{
		APIKey:     c.apiKey(),
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: sdkHTTPClient(c.TimeoutMs, streaming),
		// RetryOptions left nil: the SDK does not retry, sharedLLMTransport does.
		HTTPOptions: genai.HTTPOptions{BaseURL: base, APIVersion: version},
	}
	cl, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("gemini: client: %w", err)
	}
	return cl, nil
}

func (c *GeminiClient) model() string { return strings.TrimPrefix(c.Model, "models/") }

func (c *GeminiClient) build(req domain.GenerateRequest) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	cfg := &genai.GenerateContentConfig{}
	if s := systemText(req.System); s != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: s}}}
	}
	contents, err := toGeminiContents(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if len(req.Tools) > 0 {
		tool := &genai.Tool{}
		for _, t := range req.Tools {
			params, err := schemaMap(t.Parameters)
			if err != nil {
				return nil, nil, fmt.Errorf("gemini: tool %q: %w", t.Name, err)
			}
			tool.FunctionDeclarations = append(tool.FunctionDeclarations, &genai.FunctionDeclaration{
				Name: t.Name, Description: t.Description, ParametersJsonSchema: params,
			})
		}
		cfg.Tools = []*genai.Tool{tool}
		fc := &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}
		switch req.ToolChoice.Mode {
		case domain.ToolChoiceNone:
			fc.Mode = genai.FunctionCallingConfigModeNone
		case domain.ToolChoiceRequired:
			fc.Mode = genai.FunctionCallingConfigModeAny
		case domain.ToolChoiceTool:
			fc.Mode = genai.FunctionCallingConfigModeAny
			fc.AllowedFunctionNames = []string{req.ToolChoice.Name}
		}
		if req.ToolChoice.Mode != domain.ToolChoiceAuto {
			cfg.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: fc}
		}
	}
	if rs := req.ResponseSchema; rs != nil {
		schema, err := schemaMap(rs.Schema)
		if err != nil {
			return nil, nil, fmt.Errorf("gemini: response schema: %w", err)
		}
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseJsonSchema = schema
	}
	if n := req.Options.MaxTokens; n > 0 {
		cfg.MaxOutputTokens = int32(n)
	} else if c.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(c.MaxTokens)
	}
	if t := req.Options.Temperature; t != nil {
		v := *t
		cfg.Temperature = &v
	}
	if len(req.Options.Stop) > 0 {
		cfg.StopSequences = req.Options.Stop
	}
	if ext := providerExtensions(req, "gemini"); len(ext) > 0 {
		body := make(map[string]any, len(ext))
		for k, v := range ext {
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return nil, nil, fmt.Errorf("gemini: provider extension %q: %w", k, err)
			}
			body[k] = val
		}
		cfg.HTTPOptions = &genai.HTTPOptions{ExtraBody: body}
	}
	return contents, cfg, nil
}

// toGeminiContents renders the conversation. Gemini correlates a function response
// by NAME, not id, so a tool turn's name is recovered from the assistant call it
// answers; thoughtSignature is echoed back on the call part, which Gemini requires
// for multi-turn function calling on thinking models.
func toGeminiContents(messages []domain.ModelMessage) ([]*genai.Content, error) {
	names := map[string]string{}
	var out []*genai.Content
	appendTo := func(role string, parts ...*genai.Part) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			return
		}
		out = append(out, &genai.Content{Role: role, Parts: parts})
	}
	for _, m := range messages {
		switch m.Role {
		case domain.RoleAssistant:
			var parts []*genai.Part
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := map[string]any{}
				if len(tc.Arguments) > 0 {
					if err := json.Unmarshal(tc.Arguments, &args); err != nil {
						return nil, fmt.Errorf("gemini: tool call %q arguments: %w", tc.Name, err)
					}
				}
				names[tc.ID] = tc.Name
				parts = append(parts, &genai.Part{
					FunctionCall:     &genai.FunctionCall{ID: tc.ID, Name: tc.Name, Args: args},
					ThoughtSignature: tc.Signature,
				})
			}
			if len(parts) == 0 {
				parts = append(parts, &genai.Part{Text: ""})
			}
			appendTo(genai.RoleModel, parts...)
		case domain.RoleTool:
			resp := map[string]any{}
			if err := json.Unmarshal([]byte(m.Content), &resp); err != nil || resp == nil {
				// Not a JSON object: Gemini wants an object, so wrap the text.
				resp = map[string]any{"output": m.Content}
			}
			name := names[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID
			}
			appendTo(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID: m.ToolCallID, Name: name, Response: resp,
			}})
		default:
			appendTo(genai.RoleUser, &genai.Part{Text: m.Content})
		}
	}
	return out, nil
}

// geminiAcc folds one or more responses (a stream, or a single reply) into C1.
type geminiAcc struct {
	resp  domain.GenerateResponse
	text  strings.Builder
	calls []domain.ModelToolCall
	seen  bool
}

func (a *geminiAcc) add(r *genai.GenerateContentResponse) string {
	if r == nil {
		return ""
	}
	if r.ModelVersion != "" {
		a.resp.Model = r.ModelVersion
	}
	if u := r.UsageMetadata; u != nil {
		a.resp.Usage = domain.GenerateUsage{
			Input:     int(u.PromptTokenCount),
			Output:    int(u.CandidatesTokenCount),
			CacheRead: int(u.CachedContentTokenCount),
		}
	}
	if len(r.Candidates) == 0 {
		return ""
	}
	cand := r.Candidates[0]
	if cand.FinishReason != "" {
		a.resp.StopReason = normalizeGeminiFinishReason(cand.FinishReason)
	}
	var delta strings.Builder
	if cand.Content != nil {
		for _, p := range cand.Content.Parts {
			if p == nil {
				continue
			}
			a.seen = true
			if p.FunctionCall != nil {
				args, _ := json.Marshal(p.FunctionCall.Args)
				if p.FunctionCall.Args == nil {
					args = []byte(`{}`)
				}
				a.calls = append(a.calls, domain.ModelToolCall{
					ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Arguments: args, Signature: p.ThoughtSignature,
				})
				continue
			}
			if p.Thought || p.Text == "" {
				continue
			}
			delta.WriteString(p.Text)
		}
	}
	a.text.WriteString(delta.String())
	return delta.String()
}

func (a *geminiAcc) finish() domain.GenerateResponse {
	a.resp.Text = a.text.String()
	a.resp.ToolCalls = a.calls
	// Gemini reports STOP even on a turn that carries function calls; ModelTurn's
	// ShouldContinue checks the calls themselves (ADR-0097 D4), and naming the turn
	// tool_use here keeps the normalised reason truthful too.
	if len(a.calls) > 0 && a.resp.StopReason == domain.StopEndTurn {
		a.resp.StopReason = domain.StopToolUse
	}
	if a.resp.StopReason == "" {
		a.resp.StopReason = domain.StopUnknown
	}
	return a.resp
}

func normalizeGeminiFinishReason(r genai.FinishReason) domain.StopReason {
	switch r {
	case genai.FinishReasonStop:
		return domain.StopEndTurn
	case genai.FinishReasonMaxTokens:
		return domain.StopMaxTokens
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent, genai.FinishReasonSPII:
		return domain.StopRefusal
	default:
		return domain.StopUnknown
	}
}

// GenerateRequest implements domain.RequestGenerator.
func (c *GeminiClient) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	contents, cfg, err := c.build(req)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	cl, err := c.client(ctx, false)
	if err != nil {
		return domain.GenerateResponse{}, err
	}
	r, err := cl.Models.GenerateContent(ctx, c.model(), contents, cfg)
	if err != nil {
		return domain.GenerateResponse{}, wrapProviderError("gemini", err)
	}
	var acc geminiAcc
	acc.add(r)
	resp := acc.finish()
	resp.Ignored = ignored
	if !acc.seen && resp.StopReason != domain.StopRefusal {
		return domain.GenerateResponse{}, fmt.Errorf("gemini: empty response")
	}
	// ADR-0131 D2: hand the response's own usage to whoever metered this call.
	reportUsage(ctx, tokenUsageOf(resp.Usage))
	return resp, nil
}

// StreamRequest implements domain.RequestGenerator.
func (c *GeminiClient) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	req, ignored, err := prepareRequest(req, c.Capabilities())
	if err != nil {
		return nil, err
	}
	contents, cfg, err := c.build(req)
	if err != nil {
		return nil, err
	}
	cl, err := c.client(ctx, true)
	if err != nil {
		return nil, err
	}
	next, stop := iter.Pull2(cl.Models.GenerateContentStream(ctx, c.model(), contents, cfg))
	// Pull the first response so an HTTP refusal is returned synchronously.
	firstResp, firstErr, ok := next()
	if ok && firstErr != nil {
		stop()
		return nil, wrapProviderError("gemini", firstErr)
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		defer stop()
		var acc geminiAcc
		var serr error
		for r, e, more := firstResp, firstErr, ok; more; r, e, more = next() {
			if ctx.Err() != nil {
				return
			}
			if e != nil {
				serr = e
				break
			}
			if text := acc.add(r); text != "" {
				out <- domain.StreamChunk{Text: text}
			}
		}
		if ctx.Err() != nil {
			return
		}
		resp := acc.finish()
		resp.Ignored = ignored
		reportUsage(ctx, tokenUsageOf(resp.Usage))
		streamFinal(out, &resp, wrapStreamErr("gemini", serr))
	}()
	return out, nil
}

// Generate implements domain.Generator.
func (c *GeminiClient) Generate(ctx context.Context, prompt string) (string, error) {
	return shimGenerate(ctx, c, prompt)
}

// GenerateWithTools implements domain.ToolCallingGenerator.
func (c *GeminiClient) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	if !c.NativeToolsEnabled() {
		return domain.ModelTurn{}, fmt.Errorf("gemini: native tool-calling not enabled for model %q (set native_tools)", c.Model)
	}
	return shimGenerateWithTools(ctx, c, messages, tools)
}

// GenerateStream implements domain.LLMStreamer.
func (c *GeminiClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	return shimGenerateStream(ctx, c, prompt)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var (
	_ domain.RequestGenerator     = (*GeminiClient)(nil)
	_ domain.ToolCallingGenerator = (*GeminiClient)(nil)
	_ domain.LLMStreamer          = (*GeminiClient)(nil)
)
