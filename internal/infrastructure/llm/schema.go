package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/cambrian-sh/core/domain"
)

// Response-schema validation and repair (ADR-0139 C1, refined 2026-11-04).
//
// A generator that only best-efforts a response schema (claudebox pastes it into
// the prompt; deepseek knows json_object, not json_schema) can return JSON that
// does not conform, or prose around it. That is checked ONCE, here in the provider
// layer, rather than by every adapter or every caller: the reply is validated, and
// on failure the model gets one repair turn carrying the validator's message. A
// second failure is a typed error (domain.ErrSchemaValidation), never a quietly
// malformed answer. Schema failures never trigger provider fallback (D1).
//
// Validator: github.com/santhosh-tekuri/jsonschema/v6 — pure Go, no runtime
// dependencies beyond golang.org/x/text, full drafts 4 through 2020-12 (tool and
// agent schemas are authored by third parties against varying drafts), passes the
// official JSON-Schema-Test-Suite, and loads no remote $ref unless a loader is
// registered (none is: a schema can never make the kernel fetch a URL).
// google/jsonschema-go is already in the module graph via the MCP SDK, but it is
// pre-1.0 and validates 2020-12 only.

// schemaRepairAttempts is the number of repair turns: one, by decision (C1).
const schemaRepairAttempts = 1

// SchemaError is the typed failure after the repair turn. errors.Is(err,
// domain.ErrSchemaValidation) holds.
type SchemaError struct {
	Name   string
	Reason string
	Text   string // the last reply, for diagnosis
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("%v: %s: %s", domain.ErrSchemaValidation, e.Name, e.Reason)
}

func (e *SchemaError) Unwrap() error { return domain.ErrSchemaValidation }

var compiledSchemas sync.Map // sha256(schema) -> *jsonschema.Schema

func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	key := sha256.Sum256(raw)
	if s, ok := compiledSchemas.Load(key); ok {
		return s.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("response schema is not JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	const loc = "mem://response-schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("response schema: %w", err)
	}
	s, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("response schema does not compile: %w", err)
	}
	compiledSchemas.Store(key, s)
	return s, nil
}

// extractJSON strips what best-effort models wrap JSON in — surrounding
// whitespace and one markdown code fence — and nothing more. It does not hunt for
// JSON inside prose: a reply that needs that is a reply that failed.
func extractJSON(text string) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		if nl := strings.IndexByte(t, '\n'); nl >= 0 {
			t = t[nl+1:] // drop the language tag line ("json")
		}
		t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
	}
	return t
}

// validateReply checks text against the schema and returns the normalised JSON.
func validateReply(schema []byte, text string) (string, error) {
	s, err := compileSchema(schema)
	if err != nil {
		return "", err
	}
	clean := extractJSON(text)
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(clean))
	if err != nil {
		return "", fmt.Errorf("reply is not valid JSON: %v", err)
	}
	if err := s.Validate(inst); err != nil {
		return "", err
	}
	return clean, nil
}

// schemaGenerator is the Provider-layer decorator that enforces a best-effort
// ResponseSchema. It is the outermost layer Acquire adds, outside the spend meter,
// so the repair call is admitted and booked like any other call.
type schemaGenerator struct {
	inner domain.Generator
}

// needsCheck reports whether a response must be validated: a schema was asked
// for, the generator did not report it ignored (a model that never saw the schema
// cannot be held to it), and the turn is an answer, not a tool call.
func needsCheck(req domain.GenerateRequest, resp domain.GenerateResponse) bool {
	return req.ResponseSchema != nil && len(resp.ToolCalls) == 0 &&
		!slices.Contains(resp.Ignored, domain.FieldResponseSchema) &&
		resp.StopReason != domain.StopRefusal
}

func (s *schemaGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(s.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: %T does not implement GenerateRequest", s.inner)
	}
	resp, err := rg.GenerateRequest(ctx, req)
	if err != nil || !needsCheck(req, resp) {
		return resp, err
	}
	clean, verr := validateReply(req.ResponseSchema.Schema, resp.Text)
	if verr == nil {
		resp.Text = clean
		return resp, nil
	}
	usage := resp.Usage
	for attempt := 0; attempt < schemaRepairAttempts; attempt++ {
		repair := req
		repair.Messages = append(slices.Clone(req.Messages),
			domain.ModelMessage{Role: domain.RoleAssistant, Content: resp.Text},
			domain.UserMessage("Your previous reply did not conform to the required JSON schema: "+
				verr.Error()+"\nReply again with ONLY a JSON value that conforms to the schema — no prose, no code fence."),
		)
		resp, err = rg.GenerateRequest(ctx, repair)
		if err != nil {
			return resp, err
		}
		usage = addUsage(usage, resp.Usage)
		if clean, verr = validateReply(req.ResponseSchema.Schema, resp.Text); verr == nil {
			resp.Text, resp.Usage = clean, usage
			return resp, nil
		}
	}
	resp.Usage = usage
	return resp, &SchemaError{Name: schemaName(req.ResponseSchema), Reason: verr.Error(), Text: truncate(resp.Text, 512)}
}

// StreamRequest with a response schema cannot be repaired after its text has been
// emitted, so it is served as a validated GenerateRequest delivered as a stream of
// one text chunk plus the final chunk. Without a schema it passes straight through.
func (s *schemaGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(s.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: %T does not implement StreamRequest", s.inner)
	}
	if req.ResponseSchema == nil {
		return rg.StreamRequest(ctx, req)
	}
	resp, err := s.GenerateRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan domain.StreamChunk, 2)
	if resp.Text != "" {
		out <- domain.StreamChunk{Text: resp.Text}
	}
	out <- usageChunk(&resp)
	close(out)
	return out, nil
}

func (s *schemaGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(s.inner) }

func (s *schemaGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(s.inner)
	return ok
}

// The legacy ports carry no schema: forwarded untouched, including the
// capability reports (see healthGenerator on why both halves are forwarded).

func (s *schemaGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return s.inner.Generate(ctx, prompt)
}

func (s *schemaGenerator) NativeToolsEnabled() bool { return domain.SupportsToolCalling(s.inner) }

func (s *schemaGenerator) GenerateWithTools(ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition) (domain.ModelTurn, error) {
	tg, ok := s.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf("llm provider: %T does not implement native tool-calling", s.inner)
	}
	return tg.GenerateWithTools(ctx, messages, tools)
}

func (s *schemaGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := s.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: %T does not implement streaming", s.inner)
	}
	return sg.GenerateStream(ctx, prompt)
}

func addUsage(a, b domain.GenerateUsage) domain.GenerateUsage {
	return domain.GenerateUsage{
		Input:           a.Input + b.Input,
		Output:          a.Output + b.Output,
		CacheRead:       a.CacheRead + b.CacheRead,
		CacheWrite:      a.CacheWrite + b.CacheWrite,
		CacheWriteKnown: a.CacheWriteKnown && b.CacheWriteKnown,
		Estimated:       a.Estimated || b.Estimated,
	}
}
