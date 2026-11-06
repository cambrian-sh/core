package network

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/stepoutput"
)

// The gateway producer of the live step-output stream (ADR-0139 amendment
// 2026-11-06; internal/stepoutput holds the stream and its policy).
//
// Both model lanes forward through one gatewayForwarder:
//   - GenerateViaModelStream: each text chunk the agent receives;
//   - GenerateWithTools: the turn is served over the provider's stream internally
//     (GenerateWithToolsObserved) — the RPC stays unary for the SDK — and the
//     forwarder passes on text deltas plus the configured final-answer tool's
//     answer argument, decoded incrementally from the streamed tool-call JSON.
//
// The forwarder exists only when the stream says gateway deltas may flow for the
// step (not disabled, no post_model hook whose scope matches the step, the agent
// does not push its own output);
// otherwise the lanes behave exactly as before, unary included.

// gatewayForwarder turns one model call's deltas into step-output chunks.
type gatewayForwarder struct {
	stream *stepoutput.Stream
	lease  domain.LeaseID
	tool   string
	arg    string
	calls  map[int]*forwardedCall
}

type forwardedCall struct {
	name string
	ex   *stepoutput.Extractor
}

// stepOutputForwarder returns the forwarder for a call under lease, or nil when no
// gateway delta may flow for it. Called after the lease-ownership check.
func (s *Server) stepOutputForwarder(ctx context.Context, lease domain.LeaseID) *gatewayForwarder {
	if s.StepOutput == nil {
		return nil
	}
	if reason := s.StepOutput.SuppressReason(lease, callerAgentHeader(ctx)); reason != "" {
		slog.Debug("step output: gateway deltas suppressed for this call", "lease", lease, "reason", reason)
		return nil
	}
	return &gatewayForwarder{
		stream: s.StepOutput,
		lease:  lease,
		tool:   s.StepOutput.FinalAnswerTool(),
		arg:    s.StepOutput.FinalAnswerArg(),
	}
}

// text forwards one text delta.
func (f *gatewayForwarder) text(t string) {
	if f == nil {
		return
	}
	f.stream.EmitGateway(f.lease, domain.StepOutputText, t)
}

// observe is the ModelStreamDelta observer of a native tool turn. It runs on the
// call's goroutine; EmitGateway never blocks.
func (f *gatewayForwarder) observe(d domain.ModelStreamDelta) {
	switch {
	case d.Restarted:
		// The provider failed over mid-stream: what was forwarded belongs to an
		// abandoned attempt. Say so, and start every call's extraction afresh.
		f.calls = nil
		f.stream.EmitGateway(f.lease, domain.StepOutputStatus,
			"model call restarted on a fallback model; the output above is superseded")
	case d.ToolCall != nil:
		f.toolDelta(*d.ToolCall)
	case d.Text != "":
		f.text(d.Text)
	}
}

func (f *gatewayForwarder) toolDelta(d domain.ToolCallDelta) {
	if f.calls == nil {
		f.calls = map[int]*forwardedCall{}
	}
	c := f.calls[d.Index]
	if c == nil {
		c = &forwardedCall{}
		f.calls[d.Index] = c
	}
	if d.Name != "" {
		c.name = d.Name
	}
	if c.name != f.tool || d.ArgumentsDelta == "" {
		return
	}
	if c.ex == nil {
		c.ex = stepoutput.NewExtractor(f.arg)
	}
	f.text(c.ex.Feed(d.ArgumentsDelta))
}

// generateWithTools serves one native tool turn, forwarding its live output when
// fwd is non-nil and the gateway can stream it.
func generateWithTools(ctx context.Context, gw LLMGateway, lease domain.LeaseID, req domain.GenerateRequest, fwd *gatewayForwarder) (domain.GenerateResponse, error) {
	if og, ok := gw.(ObservedToolGateway); ok && fwd != nil {
		return og.GenerateWithToolsObserved(ctx, lease, req, fwd.observe)
	}
	return gw.GenerateWithTools(ctx, lease, req)
}

// EmitStepOutput is the agent producer (ADR-0139 amendment 2026-11-06): an agent
// that calls its own model pushes its step's live output, chunk by chunk, on one
// client stream. Each chunk names the lease; the lease must be LIVE and bound to
// the caller (ADR-0139 D8) — a dead, unknown or foreign lease is PermissionDenied,
// one answer for all three so a caller learns nothing about someone else's lease.
// The check runs per chunk, because one stream may outlive a lease (a step that
// ends) and nothing stops a client from switching leases mid-stream.
//
// Duplicates (seq at or below the highest accepted) and chunks after the agent's
// final one are dropped and counted, not refused: a retried stream must be safe.
func (s *Server) EmitStepOutput(stream grpc.ClientStreamingServer[pb.StepOutputChunk, pb.EmitStepOutputAck]) error {
	if s.StepOutput == nil {
		return status.Error(codes.Unimplemented, "EmitStepOutput: the step-output stream is not wired")
	}
	ctx := stream.Context()
	ack := &pb.EmitStepOutputAck{}
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(ack)
		}
		if err != nil {
			return err
		}
		lease := domain.LeaseID(c.GetLeaseId())
		if lease == "" {
			return status.Error(codes.InvalidArgument, "lease_id is required on every chunk")
		}
		if c.GetSeq() == 0 {
			return status.Error(codes.InvalidArgument, "seq must start at 1 and increase per lease")
		}
		if _, ok := s.resolveLeaseFor(ctx, lease); !ok {
			return status.Error(codes.PermissionDenied, "lease is not valid for this caller")
		}
		switch s.StepOutput.EmitAgent(lease, stepoutput.AgentChunk{
			Seq:   c.GetSeq(),
			Kind:  stepOutputKind(c.GetKind()),
			Data:  c.GetData(),
			Final: c.GetFinal(),
		}) {
		case stepoutput.AgentAccepted:
			ack.Accepted++
			if c.GetSeq() > ack.LastSeq {
				ack.LastSeq = c.GetSeq()
			}
		case stepoutput.AgentDuplicate:
			ack.Duplicates++
		case stepoutput.AgentOverCap:
			ack.OverCap++
		case stepoutput.AgentClosed:
			ack.AfterFinal++
		}
	}
}

// stepOutputKind maps the wire enum; UNSPECIFIED (and anything unknown) is text.
func stepOutputKind(k pb.StepOutputKind) domain.StepOutputKind {
	switch k {
	case pb.StepOutputKind_STEP_OUTPUT_KIND_PROGRESS:
		return domain.StepOutputProgress
	case pb.StepOutputKind_STEP_OUTPUT_KIND_STATUS:
		return domain.StepOutputStatus
	}
	return domain.StepOutputText
}
