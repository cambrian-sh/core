package hooks

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// naturalPoints are the points a built-in kind attaches to when the declaration
// names none, and the only points it may name.
var naturalPoints = map[string][]domain.HookPoint{
	config.HookKindDenyArgsRegex:   {domain.HookPreTool},
	config.HookKindRequireApproval: {domain.HookPreTool},
	config.HookKindRedactOutput:    {domain.HookPostTool},
}

var allowedPoints = map[string]map[domain.HookPoint]bool{
	config.HookKindDenyArgsRegex:   {domain.HookPreTool: true},
	config.HookKindRequireApproval: {domain.HookPreTool: true},
	config.HookKindRedactOutput:    {domain.HookPostTool: true, domain.HookPostModel: true},
}

// FromConfig builds the operator's hooks from execution.hooks. Every error is a
// boot error: a declared safety control that did not install must not degrade
// silently (AGENTS.md §3.2).
func FromConfig(cfg config.HooksConfig) ([]domain.Hook, error) {
	defTimeout := time.Duration(cfg.DefaultTimeoutMs) * time.Millisecond
	if defTimeout <= 0 {
		defTimeout = domain.DefaultHookTimeout
	}
	out := make([]domain.Hook, 0, len(cfg.Entries))
	for _, e := range cfg.Entries {
		spec := domain.HookSpec{
			Name:       e.Name,
			Priority:   e.Priority,
			MayRewrite: e.MayRewrite,
			OnFailure:  domain.HookFailureMode(e.OnFailure),
			Timeout:    time.Duration(e.TimeoutMs) * time.Millisecond,
			Source:     "config",
			// Static scope (HookSpec.Matches): checked by the chain before the hook
			// runs, and by the live step-output stream before a reply exists.
			Agents:   append([]string(nil), e.Agents...),
			Purposes: append([]string(nil), e.Purposes...),
		}
		if spec.Timeout <= 0 {
			spec.Timeout = defTimeout
		}
		for _, p := range e.Points {
			spec.Points = append(spec.Points, domain.HookPoint(p))
		}
		if len(spec.Points) == 0 {
			spec.Points = naturalPoints[e.Kind]
		}
		if allowed, builtin := allowedPoints[e.Kind]; builtin {
			for _, p := range spec.Points {
				if !allowed[p] {
					return nil, fmt.Errorf("hook %q: kind %s cannot attach to %s", e.Name, e.Kind, p)
				}
			}
		}
		var (
			h   domain.Hook
			err error
		)
		switch e.Kind {
		case config.HookKindDenyArgsRegex:
			h, err = NewDenyArgsRegex(spec, e.Tools, e.Pattern, e.Reason)
		case config.HookKindRequireApproval:
			h = NewRequireApproval(spec, e.Tools, e.Reason)
		case config.HookKindRedactOutput:
			h, err = NewRedactOutput(spec, e.Tools, e.Pattern, e.Replacement)
		case config.HookKindCommand:
			h, err = NewCommand(spec, e.Command, e.Env)
		default:
			err = fmt.Errorf("hook %q: unknown kind %q", e.Name, e.Kind)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// NewChain is the composition-root entry: the operator's config hooks plus the
// plugins' hooks, validated and ordered into one chain. observer may be nil.
func NewChain(cfg config.HooksConfig, pluginHooks []domain.Hook, observer domain.HookObserver, log *slog.Logger) (*domain.HookChain, error) {
	hs, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}
	hs = append(hs, pluginHooks...)
	return domain.NewHookChain(hs, observer, log)
}
