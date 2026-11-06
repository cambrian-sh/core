package llm

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// T-20261106-thought-steps-call-the-model-under-the-planner: in-kernel thought
// steps carry their own purpose, `thought` — visible to hooks and the spend meter —
// while routing exactly like the planner unless a `thought` role is configured.

// thoughtProvider: default "d", planner role "p", and (when thoughtRole) a thought
// role "t", each its own server so the wire shows which generator served a call.
func thoughtProvider(t *testing.T, d, pl, th *bodyServer, thoughtRole bool, hs ...domain.Hook) *Provider {
	t.Helper()
	roles := map[string]string{"planner": "p"}
	if thoughtRole {
		roles["thought"] = "t"
	}
	gen := func(id string, bs *bodyServer) config.GeneratorConfig {
		return config.GeneratorConfig{ID: id, Provider: "openai", Model: "m-" + id, Endpoint: bs.srv.URL, TimeoutMs: 5000}
	}
	var opts []ProviderOption
	if len(hs) > 0 {
		chain, err := domain.NewHookChain(hs, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, WithHooks(chain))
	}
	p, err := NewProvider(config.LLMProviderConfig{
		Default:    "d",
		Generators: []config.GeneratorConfig{gen("d", d), gen("p", pl), gen("t", th)},
		Roles:      roles,
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func thoughtCall(t *testing.T, p *Provider) string {
	t.Helper()
	resp, err := p.GeneratorFor(domain.PurposeThought).(domain.RequestGenerator).
		GenerateRequest(context.Background(), domain.UserPrompt("synthesise"))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Text
}

func TestThoughtPurpose_RoutesLikeThePlannerUnlessAThoughtRoleIsConfigured(t *testing.T) {
	t.Parallel()
	t.Run("no thought role: the planner's generator", func(t *testing.T) {
		t.Parallel()
		d, pl, th := newBodyServer(t, `"d"`), newBodyServer(t, `"p"`), newBodyServer(t, `"t"`)
		p := thoughtProvider(t, d, pl, th, false)
		id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeThought})
		if err != nil || id != "p" {
			t.Fatalf("resolve(thought) = %q %v, want the planner's p", id, err)
		}
		if got := thoughtCall(t, p); got != "p" {
			t.Fatalf("thought call served by %q, want p (planner)", got)
		}
		if _, listed := p.Roles()["thought"]; listed {
			t.Fatal("an unbound thought purpose must not appear as a configured role")
		}
	})
	t.Run("thought role configured: its own generator", func(t *testing.T) {
		t.Parallel()
		d, pl, th := newBodyServer(t, `"d"`), newBodyServer(t, `"p"`), newBodyServer(t, `"t"`)
		p := thoughtProvider(t, d, pl, th, true)
		if got := thoughtCall(t, p); got != "t" {
			t.Fatalf("thought call served by %q, want t", got)
		}
		// The planner itself is untouched by the thought binding.
		id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposePlanner})
		if err != nil || id != "p" {
			t.Fatalf("resolve(planner) = %q %v, want p", id, err)
		}
	})
	t.Run("a live SetRole(thought) takes over from the fallback", func(t *testing.T) {
		t.Parallel()
		d, pl, th := newBodyServer(t, `"d"`), newBodyServer(t, `"p"`), newBodyServer(t, `"t"`)
		p := thoughtProvider(t, d, pl, th, false)
		p.SetRole("thought", "t")
		if got := thoughtCall(t, p); got != "t" {
			t.Fatalf("after SetRole: served by %q, want t", got)
		}
	})
}

// purposeRecorder is a post_model hook scoped to purposes; it records the purpose
// of every event it is called for.
type purposeRecorder struct {
	name     string
	purposes []string
	mu       sync.Mutex
	seen     []string
}

func (h *purposeRecorder) Spec() domain.HookSpec {
	return domain.HookSpec{Name: h.name, Points: []domain.HookPoint{domain.HookPreModel, domain.HookPostModel}, Purposes: h.purposes}
}

func (h *purposeRecorder) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, string(ev.Point)+":"+ev.Model.Purpose)
	return domain.HookResult{}, nil
}

func (h *purposeRecorder) got() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.seen, ",")
}

func TestThoughtPurpose_ReachesHooksAsThought(t *testing.T) {
	t.Parallel()
	d, pl, th := newBodyServer(t, `"d"`), newBodyServer(t, `"p"`), newBodyServer(t, `"t"`)
	agentOnly := &purposeRecorder{name: "agent-only", purposes: []string{"agent_step"}}
	deliverables := &purposeRecorder{name: "deliverables", purposes: []string{"agent_step", "thought"}}
	plannerOnly := &purposeRecorder{name: "planner-only", purposes: []string{"planner"}}
	p := thoughtProvider(t, d, pl, th, false, agentOnly, deliverables, plannerOnly)

	thoughtCall(t, p)

	if got := agentOnly.got(); got != "" {
		t.Errorf("an [agent_step] hook saw a thought call: %s", got)
	}
	if got := plannerOnly.got(); got != "" {
		t.Errorf("a [planner] hook saw a thought call (it would rewrite deliverables): %s", got)
	}
	if got, want := deliverables.got(), "pre_model:thought,post_model:thought"; got != want {
		t.Errorf("[agent_step, thought] hook saw %q, want %q", got, want)
	}

	// And a planner call still reaches the planner-scoped hook only.
	if _, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	if got, want := plannerOnly.got(), "pre_model:planner,post_model:planner"; got != want {
		t.Errorf("[planner] hook saw %q, want %q", got, want)
	}
	if got := deliverables.got(); strings.Contains(got, "planner") {
		t.Errorf("deliverables hook saw a planner call: %s", got)
	}
}

func TestThoughtPurpose_SpendIsRecordedUnderThought(t *testing.T) {
	t.Parallel()
	d, pl, th := newBodyServer(t, `"d"`), newBodyServer(t, `"p"`), newBodyServer(t, `"t"`)
	p := thoughtProvider(t, d, pl, th, false)
	p.SetSpendMeter(NewSpendMeter(0, p.Ledger(), nil))

	ctx, tally := domain.WithLLMUsageTally(context.Background())
	if _, err := p.GeneratorFor(domain.PurposeThought).(domain.RequestGenerator).
		GenerateRequest(ctx, domain.UserPrompt("synthesise")); err != nil {
		t.Fatal(err)
	}
	got := tally.ByPurpose()
	if len(got) != 1 || got[0].Purpose != string(domain.PurposeThought) || got[0].Usage.Calls != 1 {
		t.Fatalf("tally = %+v, want one call under thought", got)
	}
}
