package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 D11 pre_model / post_model hooks on the Provider's request path.
//
// They run at the Provider's entry points (the purpose- and model-bound handles,
// and the agent-plane GenerateOn/StreamOn chain), OUTSIDE the fallback walk: a
// hook sees one logical call, not one per generator attempt, and a hook's refusal
// (domain.ErrHookDenied) is never mistaken for an endpoint failure.
//
//   - pre_model may rewrite the messages (may_rewrite) and add context: hook
//     annotations are appended to the request as one trailing system block.
//   - post_model sees the whole reply — text, tool calls, the native tool
//     lane's deliverable (the configured final-answer call's answer argument) and
//     the configured guarded control-verb arguments (yield_subgoal.intent,
//     delegate_to_planner.task, …) — and may rewrite the text, the deliverable
//     and the guarded arguments (may_rewrite) or refuse the reply, tool calls
//     included. A rewritten argument is written back into that call's JSON
//     arguments; the other calls are left to the ToolExecutor's pre_tool hooks.
//   - a hook scoped by agents/purposes (HookSpec.Matches) runs only for the calls
//     it covers; the event's purpose is the label below (lrLabel, or agent_step
//     on the agent plane) and its agent the context's agent principal.
//   - A streamed call (StreamOn, a handle's StreamRequest/GenerateStream) that a
//     post_model hook's scope MATCHES — decided statically, before the call, by
//     HookChain.HasFor with the call's purpose label and agent — is served
//     BUFFERED (bufferedStream, T-20261106-text-protocol-agent-replies-never-reach-post):
//     the provider stream is consumed internally, post_model runs on the complete
//     reply, and only the (possibly rewritten) text is delivered, as a text chunk
//     plus the final usage/Response chunk. A refusal or a broken stream delivers
//     nothing. A streamed call no post_model hook matches streams live and fires
//     pre_model only — chunks already delivered cannot be rewritten or withheld.
//     On the text lane the whole reply text is the hook's subject: guarded
//     control-verb arguments exist only as structured tool calls (native lane);
//     a text-protocol final answer or control-verb envelope is inside the text, so
//     a redact_output or deny pattern over the text covers it.
//   - The model path has no approval channel, so a hook narrowing to ask is
//     refused like a deny.

// ProviderOption configures a Provider at construction.
type ProviderOption func(*Provider)

// WithHooks installs the D11 hook chain. Constructor-injected (AGENTS.md §2.3):
// the Provider is never in a hooked-later state. nil ⇒ no hooks.
func WithHooks(chain *domain.HookChain) ProviderOption {
	return func(p *Provider) { p.hooks = chain }
}

// WithFinalAnswer names the native tool lane's deliverable for post_model hooks:
// the tool whose string argument arg is the step's answer
// (execution.step_output.final_answer_tool / final_answer_arg — the SDK's control
// vocabulary, so configuration, not code). Unset ⇒ the config defaults.
func WithFinalAnswer(tool, arg string) ProviderOption {
	return func(p *Provider) {
		if tool != "" {
			p.finalAnswerTool = tool
		}
		if arg != "" {
			p.finalAnswerArg = arg
		}
	}
}

// WithGuardedArgs names the control-verb arguments post_model hooks see and may
// rewrite besides the deliverable (execution.hooks.guarded_arguments — the SDK's
// control vocabulary, so configuration, not code). nil ⇒ the config defaults; a
// non-nil list replaces them.
func WithGuardedArgs(guards []domain.GuardedArg) ProviderOption {
	return func(p *Provider) {
		if guards != nil {
			p.guardedArgs = append([]domain.GuardedArg(nil), guards...)
		}
	}
}

// Hooks returns the installed hook chain (nil when none).
func (p *Provider) Hooks() *domain.HookChain { return p.hooks }

// hookAgentID is the agent a model hook event is attributed to: the context's
// agent principal (the agent plane's interceptor sets it from x-agent-id), else "".
func hookAgentID(ctx context.Context) string {
	if pr := domain.PrincipalFromContext(ctx); pr.Kind == domain.PrincipalAgent {
		return pr.ID
	}
	return ""
}

// postModelMatches reports whether a post_model hook would run for a call of
// purpose on ctx — the hooks' static scope (HookSpec.Matches), decidable before the
// reply exists. It is the switch between a live stream and a buffered one.
func (p *Provider) postModelMatches(ctx context.Context, purpose string) bool {
	return p.hooks.HasFor(domain.HookPostModel, hookAgentID(ctx), purpose)
}

// discardDelta is the observer of a buffered stream: nothing is forwarded live.
func discardDelta(domain.ModelStreamDelta) {}

// bufferedStream serves a streamed C1 call whose reply a post_model hook must see
// (the matching case above). walk runs one attempt per candidate — the same
// fallback walk the live call uses; since nothing has been delivered, a stream that
// breaks mid-way is an endpoint failure and the next candidate serves, exactly as
// in GenerateOnObserved. The reply then crosses post_model, and the returned
// channel is already filled and closed: no goroutine, nothing delivered before the
// hooks ran, nothing at all on an error or a refusal (fail closed). pre_model has
// already run (the caller's, with streamed=false: to the hooks this call is not
// streamed).
func (p *Provider) bufferedStream(ctx context.Context, label, model string, req domain.GenerateRequest, walk func(call func(domain.Generator) error) (string, error)) (<-chan domain.StreamChunk, string, error) {
	var out domain.GenerateResponse
	id, err := walk(func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement StreamRequest", gen)
		}
		resp, _, err := consumeObservedStream(ctx, rg, req, discardDelta)
		if err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return nil, id, err
	}
	if err := p.postModel(ctx, label, model, req, &out.Text, &out.ToolCalls); err != nil {
		return nil, id, err
	}
	return replyChunks(out), id, nil
}

// replyChunks is a complete reply as a closed stream in the C1 shape: the text as
// one chunk (when there is any), then the final chunk with the usage and the whole
// Response — the same final chunk an adapter sends (usageChunk), so a consumer
// reading the stop reason, Ignored or the model loses nothing.
func replyChunks(resp domain.GenerateResponse) <-chan domain.StreamChunk {
	ch := make(chan domain.StreamChunk, 2)
	if resp.Text != "" {
		ch <- domain.StreamChunk{Text: resp.Text}
	}
	ch <- usageChunk(&resp)
	close(ch)
	return ch
}

// bufferedPromptStream is bufferedStream for the legacy single-string streaming
// port (GenerateStream): its chunks carry text and, on the final one, usage — no
// Response — and a failure is the stream_error convention (a "stream_error: "
// text). The reply is assembled, post_model runs on the text, and the text is
// delivered as one chunk followed by the final usage chunk.
func (p *Provider) bufferedPromptStream(ctx context.Context, lr domain.LLMRequest, prompt string) (<-chan domain.StreamChunk, error) {
	label, model := lrLabel(lr), lr.SuggestedModelID
	var text string
	var final domain.StreamChunk
	err := p.withFallback(ctx, lr, func(gen domain.Generator) error {
		sg, ok := gen.(streamingInner)
		if !ok {
			return fmt.Errorf("llm provider: generator %T does not implement streaming", gen)
		}
		ch, err := sg.GenerateStream(ctx, prompt)
		if err != nil {
			return err
		}
		var b strings.Builder
		var last domain.StreamChunk
		var streamErr string
		for c := range ch { // always drained, so every layer below closes
			if streamErr != "" {
				continue
			}
			if strings.HasPrefix(c.Text, "stream_error: ") {
				streamErr = strings.TrimPrefix(c.Text, "stream_error: ")
				continue
			}
			b.WriteString(c.Text)
			if c.IsFinal {
				last = c
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if streamErr != "" {
			return errors.New(streamErr)
		}
		text, final = b.String(), last
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := p.postModel(ctx, label, model, domain.UserPrompt(prompt), &text, nil); err != nil {
		return nil, err
	}
	ch := make(chan domain.StreamChunk, 2)
	if text != "" {
		ch <- domain.StreamChunk{Text: text}
	}
	final.Text, final.IsFinal = "", true
	ch <- final
	close(ch)
	return ch, nil
}

// modelHookEvent builds the event for one model call.
func modelHookEvent(ctx context.Context, point domain.HookPoint, purpose, model string, req domain.GenerateRequest, streamed bool) domain.HookEvent {
	call := &domain.HookModelCall{Purpose: purpose, Model: model, Messages: req.Messages, Streamed: streamed}
	for _, b := range req.System {
		call.System = append(call.System, b.Text)
	}
	for _, t := range req.Tools {
		call.Tools = append(call.Tools, t.Name)
	}
	ev := domain.HookEvent{
		Point:      point,
		Principals: domain.HookPrincipalChain(ctx, ""),
		Model:      call,
		Verdict:    domain.HookAllow,
	}
	if sid, ok := domain.SessionIDFromContext(ctx); ok {
		ev.RunID = string(sid)
	}
	ev.AgentID = hookAgentID(ctx)
	return ev
}

// preModel runs the pre_model hooks for req, applying their rewrites in place.
func (p *Provider) preModel(ctx context.Context, purpose, model string, req *domain.GenerateRequest, streamed bool) error {
	if !p.hooks.Has(domain.HookPreModel) {
		return nil
	}
	out := p.hooks.Run(ctx, modelHookEvent(ctx, domain.HookPreModel, purpose, model, *req, streamed))
	if out.Verdict != domain.HookAllow {
		return fmt.Errorf("llm provider: %w: %s", domain.ErrHookDenied, out.DenyReason())
	}
	if out.MessagesRewritten {
		req.Messages = out.Messages
	}
	if len(out.Annotations) > 0 {
		// A copy, so a caller's System slice is never appended into.
		sys := make([]domain.SystemBlock, 0, len(req.System)+1)
		sys = append(sys, req.System...)
		req.System = append(sys, domain.SystemBlock{Text: strings.Join(out.Annotations, "\n")})
	}
	return nil
}

// postModel runs the post_model hooks over a reply — its text and, when calls is
// non-nil, its tool calls — rewriting the text and the deliverable in place. A
// refusal refuses the whole reply: the caller returns no text and no tool call.
func (p *Provider) postModel(ctx context.Context, purpose, model string, req domain.GenerateRequest, text *string, calls *[]domain.ModelToolCall) error {
	if !p.hooks.Has(domain.HookPostModel) {
		return nil
	}
	ev := modelHookEvent(ctx, domain.HookPostModel, purpose, model, req, false)
	ev.Text = *text
	if calls != nil {
		ev.ToolCalls, ev.FinalAnswer = domain.HookReplyView(*calls, p.finalAnswerTool, p.finalAnswerArg)
		ev.GuardedArgs = domain.HookGuardedView(*calls, p.finalAnswerTool, p.finalAnswerArg, p.guardedArgs)
	}
	out := p.hooks.Run(ctx, ev)
	if out.Verdict != domain.HookAllow {
		return fmt.Errorf("llm provider: %w: %s", domain.ErrHookDenied, out.DenyReason())
	}
	if out.TextRewritten {
		*text = out.Text
	}
	if out.GuardedArgsRewritten {
		if calls == nil || len(out.ToolCalls) != len(*calls) {
			// Cannot happen through the chain; if it ever does, a rewrite that cannot
			// be applied must refuse, not pass the original (fail closed).
			return fmt.Errorf("llm provider: %w: a guarded-argument rewrite does not fit the reply", domain.ErrHookDenied)
		}
		// A copy of the slice: the generator's response is never written into. The
		// chain only ever changes guarded arguments, so every call's arguments are
		// taken from its view (the rest are byte-for-byte copies).
		rewritten := append([]domain.ModelToolCall(nil), (*calls)...)
		for i := range rewritten {
			rewritten[i].Arguments = append([]byte(nil), out.ToolCalls[i].Args...)
		}
		*calls = rewritten
	}
	return nil
}

// prePrompt is preModel for the legacy single-string ports: the prompt is one
// user message; a rewrite or annotation folds back into one string.
func (p *Provider) prePrompt(ctx context.Context, purpose, model, prompt string, streamed bool) (string, error) {
	if !p.hooks.Has(domain.HookPreModel) {
		return prompt, nil
	}
	req := domain.UserPrompt(prompt)
	if err := p.preModel(ctx, purpose, model, &req, streamed); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.System)+len(req.Messages))
	for _, b := range req.System {
		parts = append(parts, b.Text)
	}
	for _, m := range req.Messages {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n\n"), nil
}

// lrLabel names an LLMRequest for a hook event: its purpose, or "model" for a
// model-bound handle.
func lrLabel(lr domain.LLMRequest) string {
	if lr.Purpose != "" {
		return string(lr.Purpose)
	}
	return "model"
}

// withLeadingContext prefixes the hook-added system text to the first user turn,
// or prepends a user turn when the conversation does not start with one.
func withLeadingContext(messages []domain.ModelMessage, sys []domain.SystemBlock) []domain.ModelMessage {
	texts := make([]string, 0, len(sys))
	for _, b := range sys {
		texts = append(texts, b.Text)
	}
	lead := strings.Join(texts, "\n")
	out := make([]domain.ModelMessage, 0, len(messages)+1)
	if len(messages) > 0 && messages[0].Role == domain.RoleUser {
		first := messages[0]
		first.Content = lead + "\n\n" + first.Content
		out = append(out, first)
		return append(out, messages[1:]...)
	}
	out = append(out, domain.UserMessage(lead))
	return append(out, messages...)
}
