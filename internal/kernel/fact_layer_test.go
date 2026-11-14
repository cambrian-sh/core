package kernel

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0136 A1 composition: the fact layer's substrate exists only behind the enforcing
// writer and reader built in NewMemoryStack, the extractor exists only when extraction is
// on, and every execution.facts / ingestion fact field reaches its consumer (AGENTS.md
// §3.4).

// plainVec is a VectorStore; NewMemoryStack's construction calls none of its methods.
type plainVec struct{ domain.VectorStore }

// nopSubstrate / nopReader satisfy the raw fact ports; construction calls none of them.
type nopSubstrate struct{ domain.FactSubstrateStore }

type nopResetter struct{}

func (nopResetter) ResetFacts(context.Context, domain.FactResetFilter) (domain.FactResetReport, error) {
	return domain.FactResetReport{}, nil
}

type nopReader struct{ domain.FactItemReader }

type nopLister struct{}

func (nopLister) ListFactBackfillCandidates(context.Context, domain.FactBackfillFilter) ([]domain.FactBackfillCandidate, error) {
	return nil, nil
}

func TestMemoryStack_FactSubstrateIsEnforcedAndExtractorGated(t *testing.T) {
	cfg := config.DefaultConfig()
	// Extraction ships ON since ENG-1496; this test drives the switch itself.
	cfg.Execution.Facts.ExtractEnabled = false
	sub := FactSubstrate{Store: nopSubstrate{}, Reader: nopReader{}, Lister: nopLister{}, EmbedModelID: "m"}

	off := NewMemoryStack(plainVec{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, sub)
	if _, ok := off.FactWriter.(*authz.EnforcingKnowledgeWriter); !ok {
		t.Fatalf("FactWriter = %T, want the enforcing writer", off.FactWriter)
	}
	if _, ok := off.FactReader.(*authz.EnforcingFactItemReader); !ok {
		t.Fatalf("FactReader = %T, want the enforcing reader", off.FactReader)
	}
	if !off.QueryService.HasFactReader() {
		t.Fatal("the QueryService must hold the enforcing fact reader (constructor argument)")
	}
	if off.FactExtractor != nil || off.FactBackfill != nil {
		t.Fatal("extraction off: no extractor, no backfill, no goroutine")
	}
	if off.FactReset != nil {
		t.Fatal("the kernel never holds a fact reset; only cmd/facts-reset hands one in")
	}
	withReset := sub
	withReset.Resetter = nopResetter{}
	if r := NewMemoryStack(plainVec{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, withReset); r.FactReset == nil || r.FactExtractor != nil {
		t.Fatal("a handed-in resetter reaches MemoryStack.FactReset (ADR-0136 A2-Q3), with extraction still off")
	}

	cfg.Execution.Facts.ExtractEnabled = true
	on := NewMemoryStack(plainVec{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, sub)
	if on.FactExtractor == nil || on.FactBackfill == nil {
		t.Fatal("extract_enabled must construct the extractor and (with a lister) the backfill")
	}
	// The extractor's own lifecycle (Start/Stop, goleak) is covered in internal/memory;
	// MemoryStack.Start/Shutdown call it nil-safely next to the triplet batcher.
	on.FactExtractor.Start(context.Background())
	on.FactExtractor.Stop()

	// No substrate handed in: no writer, no reader, no extractor.
	none := NewMemoryStack(plainVec{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{})
	if none.FactWriter != nil || none.FactReader != nil || none.FactExtractor != nil || none.QueryService.HasFactReader() {
		t.Fatal("the zero FactSubstrate must leave the fact layer absent")
	}
}

func TestFactExtractorConfigFrom_Threads(t *testing.T) {
	got := factExtractorConfigFrom(config.FactsConfig{
		QueueSize: 9, Workers: 3, LLMTimeoutMs: 1500, LLMTimeoutPerKCharMs: 40, LLMTimeoutMaxMs: 9000, ParseRetries: 2, MaxFactsPerDocument: 7, MaxPromptChars: 9000,
		SingleAttributes: []string{"home_city"}, MultiAttributes: []string{"likes"}, SupersessionEnabled: true,
		AssessmentsEnabled: true, FuzzyCandidates: 4, CardinalityMinVotes: 5,
		MaxCallsPerDocument: 3, MaxFactsPerSource: 99,
	})
	if got.QueueSize != 9 || got.Workers != 3 || got.LLMTimeout != 1500*time.Millisecond ||
		got.LLMTimeoutPerKChar != 40*time.Millisecond || got.LLMTimeoutMax != 9000*time.Millisecond || got.ParseRetries != 2 ||
		got.MaxFactsPerDocument != 7 || got.MaxPromptChars != 9000 || !got.Supersession || !got.Assessments ||
		got.FuzzyCandidates != 4 || got.CardinalityMinVotes != 5 ||
		got.MaxCallsPerDocument != 3 || got.MaxFactsPerSource != 99 ||
		!slices.Equal(got.SingleAttributes, []string{"home_city"}) || !slices.Equal(got.MultiAttributes, []string{"likes"}) {
		t.Fatalf("facts settings not threaded: %+v", got)
	}
	// Assessments need supersession (the judge's verdicts are D5 inputs).
	if factExtractorConfigFrom(config.FactsConfig{AssessmentsEnabled: true}).Assessments {
		t.Fatal("assessments_enabled without supersession_enabled must not arm the judge")
	}
}

// ADR-0136 A2.4: the extractor version names the model behind the facts role — the
// live role binding, else the configured one, else the default generator — by its
// configured model, else its id.
func TestFactsGeneratorModel_NamesTheFactsRoleModel(t *testing.T) {
	p := config.LLMProviderConfig{
		Default:    "main",
		Generators: []config.GeneratorConfig{{ID: "main", Model: "big-model"}, {ID: "haiku", Model: "claude-haiku-4-5"}, {ID: "bare"}},
		Roles:      map[string]string{"facts": "haiku"},
	}
	if got := FactsGeneratorModel(p, nil); got != "claude-haiku-4-5" {
		t.Fatalf("configured role: %q", got)
	}
	if got := FactsGeneratorModel(p, map[string]string{"facts": "main"}); got != "big-model" {
		t.Fatalf("live role wins: %q", got)
	}
	if got := FactsGeneratorModel(p, map[string]string{"facts": "bare"}); got != "bare" {
		t.Fatalf("no model: the id: %q", got)
	}
	p.Roles = nil
	if got := FactsGeneratorModel(p, nil); got != "big-model" {
		t.Fatalf("no role: the default generator: %q", got)
	}
	if got := FactsGeneratorModel(config.LLMProviderConfig{}, nil); got != "default" {
		t.Fatalf("nothing configured: %q", got)
	}
}

func TestIngestionConfigFrom_ThreadsSyncWaitForFacts(t *testing.T) {
	got := ingestionConfigFrom(config.IngestionConfig{SyncIngestWaitForFacts: true, SyncIngestFactsWaitTimeoutMs: 2500})
	if !got.SyncWaitForFacts || got.SyncFactsWaitTimeout != 2500*time.Millisecond {
		t.Fatalf("sync facts wait not threaded: %+v", got)
	}
	def := ingestionConfigFrom(config.DefaultConfig().Execution.Ingestion)
	if def.SyncWaitForFacts || def.SyncFactsWaitTimeout != 120*time.Second {
		t.Fatalf("default: SyncWaitForFacts=%v timeout=%v, want false and 120s", def.SyncWaitForFacts, def.SyncFactsWaitTimeout)
	}
}
