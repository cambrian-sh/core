package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/stepoutput"
)

// The live step-output stream's composition (ADR-0139 amendment 2026-11-06; it
// supersedes ADR-0047 D12's token-level tier).
//
// One stream, built unconditionally next to the agent-plane server: the gateway
// producer (both model lanes) and the agent producer (EmitStepOutput) write to it;
// the operator feed (TokenChunkEvent on the live-only lane) and the MCP endpoint's
// notifications/progress read from it. Its policy — streams_output agents get no
// gateway deltas; a D11 post_model hook whose scope matches the step suppresses
// its gateway deltas, fail-closed (an unscoped one matches every step) — is
// constructor-injected here (AGENTS.md §2.3).

// stepOutputFeed is the late-bound operator-feed sink. The stream is built before
// the operator feed exists (the feed is assembled when services start), the same
// ordering progressHolder.setFeed answers; until attached, chunks have no feed
// destination (MCP subscribers still receive them).
type stepOutputFeed struct {
	emit atomic.Pointer[func(domain.DomainEvent)]
}

// attach installs the feed emitter (operator.Spool.EmitEphemeral, non-blocking).
func (f *stepOutputFeed) attach(emit func(domain.DomainEvent)) {
	if emit == nil {
		f.emit.Store(nil)
		return
	}
	f.emit.Store(&emit)
}

// sink is the stream's feed sink.
func (f *stepOutputFeed) sink(c domain.StepOutputChunk) {
	if e := f.emit.Load(); e != nil {
		(*e)(domain.TokenChunkFromStepOutput(c))
	}
}

// manifestReader is what the streams_output lookup needs from the agent registry.
type manifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// agentStreamsOutput reports whether an agent declared streams_output (profile or
// AGENT_MANIFEST). Read per call so a re-registered agent's declaration is live; a
// lookup failure reads as "does not stream" — the gateway then forwards, which is
// the pre-amendment behaviour, never a silent loss of output.
func agentStreamsOutput(reg manifestReader) func(context.Context, string) bool {
	if reg == nil {
		return nil
	}
	return func(ctx context.Context, agentID string) bool {
		m, err := reg.GetManifest(ctx, agentID)
		if err != nil || m == nil {
			return false
		}
		return m.StreamsOutput
	}
}

// buildStepOutput assembles the stream.
func buildStepOutput(cfg config.StepOutputConfig, hooks *domain.HookChain, leases domain.LeaseResolver,
	reg manifestReader, feed *stepOutputFeed) *stepoutput.Stream {
	s := stepoutput.New(stepoutput.Options{
		Config:        cfg,
		Hooks:         hooks,
		Leases:        leases,
		StreamsOutput: agentStreamsOutput(reg),
		Sinks:         []stepoutput.Sink{feed.sink},
		Log:           slog.Default(),
	})
	if !cfg.GatewayDeltasDisabled {
		// Say it at boot rather than let an operator wonder why the console's live
		// output is empty: the fail-closed rule is deliberate.
		if msg, attrs := stepOutputHookWarning(hooks); msg != "" {
			slog.Warn(msg, attrs...)
		}
	}
	return s
}

// stepOutputHookWarning describes which agent steps lose their live gateway deltas
// to post_model hooks, or "" when none does. A post_model hook suppresses a step's
// deltas only when its static scope matches the step (purpose agent_step, the
// step's agent): one scoped to other purposes (planner, router, …) suppresses
// nothing; one scoped to agents suppresses only theirs; one with no agent scope
// suppresses every step.
func stepOutputHookWarning(hooks *domain.HookChain) (string, []any) {
	var all, scoped []string
	var agents []string
	for _, s := range hooks.Specs() {
		if !s.Attaches(domain.HookPostModel) {
			continue
		}
		// Does the hook cover the gateway's purpose for SOME agent? Scope by agent
		// is checked separately below.
		probe := s
		probe.Agents = nil
		if !probe.Matches(domain.HookPostModel, "", string(domain.PurposeAgentStep)) {
			continue
		}
		if len(s.Agents) == 0 {
			all = append(all, s.Name)
			continue
		}
		scoped = append(scoped, s.Name)
		agents = append(agents, s.Agents...)
	}
	const tail = " (the hook sees only the complete reply; ADR-0139 amendment 2026-11-06). " +
		"Final step output and agent-pushed output are unaffected."
	switch {
	case len(all) > 0:
		return "step output: a post_model hook matches every agent step, so live model deltas are NOT forwarded" + tail,
			[]any{"hooks", all}
	case len(scoped) > 0:
		return "step output: post_model hooks scoped by agent: live model deltas are NOT forwarded for those agents' steps" + tail,
			[]any{"hooks", scoped, "agents", agents}
	}
	return "", nil
}

// mcpTaskOutput is the MCP endpoint's view of the stream (ADR-0126 + the
// 2026-11-06 amendment): a task id IS its session id (mcp_tasks.go), so a task's
// live output is every chunk of every lease bound to that session.
func mcpTaskOutput(s *stepoutput.Stream, cfg config.StepOutputConfig) mcpserve.TaskOutput {
	out := mcpserve.TaskOutput{
		DefaultWait: time.Duration(cfg.MCPProgressWait()) * time.Second,
		MaxWait:     time.Duration(cfg.MCPMaxWait()) * time.Second,
	}
	if s != nil {
		out.Subscribe = func(taskID string, fn func(domain.StepOutputChunk)) func() {
			return s.SubscribeSession(domain.SessionID(taskID), stepoutput.Sink(fn))
		}
	}
	return out
}
