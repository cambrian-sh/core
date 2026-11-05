package domain

import (
	"context"
	"strings"
)

// GenerateVia sends req on gen's C1 path (ADR-0139 D1) when gen genuinely is a
// RequestGenerator, and otherwise as one flattened prompt through the legacy
// Generate port.
//
// Every Provider-acquired generator is a RequestGenerator, so the fallback is for
// generators built outside the Provider (test fakes, a bare adapter). It is still
// honest: the legacy path honours no structured field, so the response lists the
// ones the request carried in Ignored — above all FieldResponseSchema, which tells
// the caller the reply was NOT validated against its schema and must be read
// tolerantly.
func GenerateVia(ctx context.Context, gen Generator, req GenerateRequest) (GenerateResponse, error) {
	if rg, ok := AsRequestGenerator(gen); ok {
		return rg.GenerateRequest(ctx, req)
	}
	text, err := gen.Generate(ctx, FlattenRequest(req))
	if err != nil {
		return GenerateResponse{}, err
	}
	resp := GenerateResponse{Text: text}
	if len(req.System) > 0 {
		resp.Ignored = append(resp.Ignored, FieldSystem)
	}
	if req.ResponseSchema != nil {
		resp.Ignored = append(resp.Ignored, FieldResponseSchema)
	}
	return resp, nil
}

// SchemaUnchecked reports whether a reply to a request with a response schema
// came back without the provider layer's validate-and-repair: the generator (or
// the legacy fallback) did not honour the schema, so the caller still has to
// tolerate prose or a code fence around the JSON.
func (r GenerateResponse) SchemaUnchecked() bool {
	for _, f := range r.Ignored {
		if f == FieldResponseSchema {
			return true
		}
	}
	return false
}

// FlattenRequest renders a structured request as the single prompt a legacy
// string-only generator receives: the system blocks, then each message, then —
// when a schema was asked for — the schema itself, so a model that cannot be
// constrained still sees the shape it is asked for.
func FlattenRequest(req GenerateRequest) string {
	var parts []string
	for _, b := range req.System {
		if t := strings.TrimSpace(b.Text); t != "" {
			parts = append(parts, t)
		}
	}
	for _, m := range req.Messages {
		if t := strings.TrimSpace(m.Content); t != "" {
			parts = append(parts, t)
		}
	}
	if req.ResponseSchema != nil && len(req.ResponseSchema.Schema) > 0 {
		parts = append(parts, "Reply with only a JSON value that conforms to this JSON Schema:\n"+string(req.ResponseSchema.Schema))
	}
	return strings.Join(parts, "\n\n")
}
