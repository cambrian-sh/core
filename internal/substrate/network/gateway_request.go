package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// The agent-plane wire ↔ C1 mapping (ADR-0139 D1, wave 1c). Both managed-call RPCs
// — GenerateViaModelStream and GenerateWithTools — build ONE domain.GenerateRequest
// here, so a field the proto carries cannot reach one lane and be dropped on the
// other (the GenerateOptions bug: T-20261104-llm-gateway-silently-drops-generateoptions-sdk).

// rpcRequest is the subset of the two request messages that maps onto C1.
type rpcRequest struct {
	system   []*pb.SystemBlock
	messages []*pb.ModelMessageProto
	prompt   string // deprecated single-string form
	tools    []*pb.ToolDefinitionProto
	schema   *pb.ResponseSchema
	options  *pb.GenerateOptions
	cacheTTL pb.CacheTTL
}

// toGenerateRequest builds the C1 request. Errors are InvalidArgument statuses.
//
// System text is carried ONLY by `system`. A message with role "system" is refused
// rather than silently re-routed: two ways to say one thing is how a cache-stable
// prefix ends up interleaved with volatile turns (ADR-0139 D2).
func (r rpcRequest) toGenerateRequest() (domain.GenerateRequest, error) {
	var req domain.GenerateRequest
	for _, b := range r.system {
		if b.GetText() == "" {
			continue
		}
		req.System = append(req.System, domain.SystemBlock{Text: b.GetText(), Cache: b.GetCache()})
	}

	for _, m := range r.messages {
		role := m.GetRole()
		switch role {
		case domain.RoleUser, domain.RoleAssistant, domain.RoleTool:
		case "system":
			return req, status.Error(codes.InvalidArgument,
				`a "system" role message is not accepted; send system text in the system field`)
		default:
			return req, status.Errorf(codes.InvalidArgument, "unknown message role %q", role)
		}
		msg := domain.ModelMessage{Role: role, Content: m.GetContent(), ToolCallID: m.GetToolCallId()}
		for _, tc := range m.GetToolCalls() {
			msg.ToolCalls = append(msg.ToolCalls, domain.ModelToolCall{
				ID:   tc.GetId(),
				Name: tc.GetName(),
				// Forwarded as the provider's own bytes: re-encoding would reorder
				// keys and change what the provider matches its record of the call
				// against.
				Arguments: []byte(tc.GetArgumentsJson()),
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	// The deprecated prompt is wrapped as a lone user turn so an un-upgraded client
	// keeps working; it is ignored once messages are sent.
	if len(req.Messages) == 0 && r.prompt != "" {
		req.Messages = []domain.ModelMessage{domain.UserMessage(r.prompt)}
	}
	if len(req.Messages) == 0 {
		return req, status.Error(codes.InvalidArgument, "messages (or the deprecated prompt) is required")
	}

	for _, t := range r.tools {
		req.Tools = append(req.Tools, domain.ToolDefinition{
			Name:        t.GetName(),
			Description: t.GetDescription(),
			Parameters:  []byte(t.GetParametersJson()),
		})
	}

	if s := r.schema; s != nil && s.GetSchemaJson() != "" {
		if !json.Valid([]byte(s.GetSchemaJson())) {
			return req, status.Error(codes.InvalidArgument, "response_schema.schema_json is not valid JSON")
		}
		req.ResponseSchema = &domain.ResponseSchema{
			Name:   s.GetName(),
			Schema: json.RawMessage(s.GetSchemaJson()),
			Strict: s.GetStrict(),
		}
	}

	if o := r.options; o != nil {
		// Field presence (proto3 optional): an unset temperature is the provider's
		// default, a set 0 is a real temperature. max_tokens 0 means "default"
		// either way, as domain.RequestOptions defines it.
		if o.MaxTokens != nil && o.GetMaxTokens() > 0 {
			req.Options.MaxTokens = int(o.GetMaxTokens())
		}
		if o.Temperature != nil {
			t := o.GetTemperature()
			req.Options.Temperature = &t
		}
		req.Options.Stop = o.GetStopSequences()
		req.Options.Strict = o.GetStrict()
	}

	switch r.cacheTTL {
	case pb.CacheTTL_CACHE_TTL_SHORT:
		req.CacheTTL = domain.CacheTTLShort
	case pb.CacheTTL_CACHE_TTL_LONG:
		req.CacheTTL = domain.CacheTTLLong
	}
	return req, nil
}

// usageProto renders C1 usage for the wire.
func usageProto(u domain.GenerateUsage) *pb.GenerateUsage {
	return &pb.GenerateUsage{
		InputTokens:      int32(u.Input),
		OutputTokens:     int32(u.Output),
		CacheReadTokens:  int32(u.CacheRead),
		CacheWriteTokens: int32(u.CacheWrite),
		CacheWriteKnown:  u.CacheWriteKnown,
		Estimated:        u.Estimated,
	}
}

// gatewayStatus maps a gateway error to the status an agent can act on.
//
//   - FailedPrecondition: the serving model cannot call tools — the documented
//     "fall back to the prompt-encoded protocol" signal, and nothing else, because
//     SDKs act on it;
//   - InvalidArgument: a field the request marked strict cannot be honoured
//     (domain.ErrUnsupported) — retrying the same request cannot succeed;
//   - Aborted: the reply failed its response_schema after the one repair;
//   - ResourceExhausted: the CONWIP gate is full, or the session spend ceiling;
//   - Unavailable: no candidate model is available (breaker open, none configured);
//   - NotFound: the lease does not exist (or has expired).
func gatewayStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, ErrToolCallingUnsupported):
		return status.Error(codes.FailedPrecondition, msg)
	case errors.Is(err, domain.ErrUnsupported):
		return status.Error(codes.InvalidArgument, msg)
	case errors.Is(err, domain.ErrSchemaValidation):
		return status.Error(codes.Aborted, msg)
	case errors.Is(err, llm.ErrSpendCeiling):
		return status.Error(codes.ResourceExhausted, msg)
	case errors.Is(err, llm.ErrNoHealthyModel), strings.HasPrefix(msg, "no model allocated"):
		return status.Error(codes.Unavailable, msg)
	case msg == "gateway_overflow":
		return status.Error(codes.ResourceExhausted, "gateway overloaded")
	case strings.HasPrefix(msg, "session not found"):
		return status.Error(codes.NotFound, msg)
	default:
		return status.Error(codes.Internal, msg)
	}
}

// renderExchangePrompt is the request side of an ADR-0079 exchange record: the
// system blocks, then every turn labelled by role, tool calls and tool results
// included, so a reviewer can reconstruct the ReAct loop from the feed. The sink
// truncates.
func renderExchangePrompt(req domain.GenerateRequest) string {
	var b strings.Builder
	for _, s := range req.System {
		b.WriteString("[system]\n")
		b.WriteString(s.Text)
		b.WriteString("\n\n")
	}
	for _, m := range req.Messages {
		if m.Role == domain.RoleTool {
			fmt.Fprintf(&b, "[tool %s]\n", m.ToolCallID)
		} else {
			fmt.Fprintf(&b, "[%s]\n", m.Role)
		}
		b.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "\n→ tool_call %s %s(%s)", tc.ID, tc.Name, tc.Arguments)
		}
		b.WriteString("\n\n")
	}
	if len(req.Tools) > 0 {
		names := make([]string, len(req.Tools))
		for i, t := range req.Tools {
			names[i] = t.Name
		}
		fmt.Fprintf(&b, "[tools offered] %s\n", strings.Join(names, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderExchangeCompletion is the response side: the prose, then each tool call
// the model asked for.
func renderExchangeCompletion(resp domain.GenerateResponse) string {
	var b strings.Builder
	b.WriteString(resp.Text)
	for _, tc := range resp.ToolCalls {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "→ tool_call %s %s(%s)", tc.ID, tc.Name, tc.Arguments)
	}
	return b.String()
}
