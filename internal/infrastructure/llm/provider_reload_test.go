package llm

import (
	"context"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ReloadGenerators is the live half of SaveGenerator (owner directive
// 2026-08-12): a reloaded table must be visible to resolution, KnowsGenerator
// and Default on the next call — no restart.
func TestProvider_ReloadGeneratorsSwapsTableLive(t *testing.T) {
	p := testProvider(t)
	if p.KnowsGenerator("claude-new") {
		t.Fatal("precondition: claude-new must not exist yet")
	}

	gens := []config.GeneratorConfig{
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://x/v1", APIKeyEnv: "K"},
		{ID: "claude-new", Provider: "openai", Model: "claude-x", Endpoint: "https://y/v1", APIKeyEnv: "K2",
			CostPer1MInput: 3, CostPer1MOutput: 15},
	}
	if err := p.ReloadGenerators(gens, "claude-new"); err != nil {
		t.Fatalf("ReloadGenerators: %v", err)
	}

	if !p.KnowsGenerator("claude-new") {
		t.Error("reloaded generator must be known live")
	}
	if p.KnowsGenerator("qwen-local") {
		t.Error("removed generator must be forgotten live")
	}
	if p.Default() != "claude-new" {
		t.Errorf("Default() = %q, want claude-new", p.Default())
	}
	// The ladder resolves a suggestion to the NEW generator.
	id, err := p.resolve(context.Background(), domain.LLMRequest{SuggestedModelID: "claude-new"})
	if err != nil || id != "claude-new" {
		t.Fatalf("resolve suggested: got %q (%v)", id, err)
	}
	// A role pointing at a now-removed generator falls to the new default
	// rather than erroring — the SetRole tolerance, preserved across reloads.
	id, err = p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeRouter})
	if err != nil || id != "claude-new" {
		t.Fatalf("orphaned role must fall to default: got %q (%v)", id, err)
	}
	// New costs are in the ledger.
	in, out, ok := p.Ledger().Cost("claude-new")
	if !ok || in != 3 || out != 15 {
		t.Errorf("ledger cost = %v/%v (%v), want 3/15", in, out, ok)
	}
}

// A bad spec must fail the reload WITHOUT touching the serving table.
func TestProvider_ReloadGeneratorsBadSpecLeavesTableUntouched(t *testing.T) {
	p := testProvider(t)
	err := p.ReloadGenerators([]config.GeneratorConfig{
		{ID: "broken", Provider: "no-such-provider", Model: "m"},
	}, "broken")
	if err == nil {
		t.Fatal("an unknown provider must fail the reload")
	}
	if !p.KnowsGenerator("deepseek") || p.Default() != "deepseek" {
		t.Error("failed reload must leave the previous table serving")
	}
}

// Concurrent resolution during reloads must be race-free (run with -race) and
// must always observe a COHERENT table: the resolved id exists in whichever
// table was current.
func TestProvider_ReloadIsRaceFreeUnderConcurrentResolve(t *testing.T) {
	p := testProvider(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, err := p.resolve(context.Background(), domain.LLMRequest{})
				if err == nil && !p.KnowsGenerator(id) {
					// Racing tables is allowed to mix WHICH table answers each of
					// the two calls — but each call individually saw a whole one,
					// so the only ids ever seen are real ids from some table.
					if id != "deepseek" && id != "alt" {
						t.Errorf("resolved unknown id %q", id)
						return
					}
				}
			}
		})
	}
	a := []config.GeneratorConfig{{ID: "deepseek", Provider: "openai", Model: "m", Endpoint: "https://x/v1"}}
	b := []config.GeneratorConfig{{ID: "alt", Provider: "openai", Model: "m", Endpoint: "https://y/v1"}}
	for i := range 200 {
		if i%2 == 0 {
			_ = p.ReloadGenerators(a, "deepseek")
		} else {
			_ = p.ReloadGenerators(b, "alt")
		}
	}
	close(stop)
	wg.Wait()
}
