// facts-backfill runs stored documents that have no memory_fact extraction marker
// through the ADR-0136 fact extractor (Amendment A1.7;
// T-20261103-no-backfill-path-populates-memory-facts-for). The fact layer extracts only
// on new ingests, so a document stored before execution.facts.extract_enabled was on has
// no facts until something feeds it to the SAME extractor.
//
// It builds its stack through the kernel builder (kernel.NewMemoryStack), never the raw
// adapter: chunk text is read through the enforcing read store with the explicit
// kernel bypass, and every item passes authz.EnforcingKnowledgeWriter, exactly as at
// ingest (memory.FactBackfill documents the access model). No proto change: it is an
// operator-run maintenance command, like cmd/chunk-fill.
//
// Cost: one extraction LLM call per window of a pending document (ADR-0136 A2.3: about
// ceil(chars / max_prompt_chars), at most execution.facts.max_calls_per_document), plus
// at most one assessment call per window with execution.facts.assessments_enabled. Run
// -dry-run first: it lists and counts and prints the estimated LLM calls without reading
// text or calling a model.
//
// Pending (A2.4): a document with no marker, a marker of another extractor version
// ("<prompt id>/<generator model>" — a changed prompt or backbone), a legacy marker, or
// a partial one (a source cut at max_calls_per_document), which resumes from its first
// uncovered window. cmd/facts-reset removes facts and markers for a clean re-run.
//
// Usage (from cambrian-core/, CAMBRIAN_CONFIG selects the config file):
//
//	go run ./cmd/facts-backfill -dry-run -namespace lme-run1-
//	go run ./cmd/facts-backfill -namespace lme-run1- -concurrency 4 -docs-per-minute 120
//	go run ./cmd/facts-backfill -namespace lme-run1- -concurrency 2 -group-concurrency 8
//
// -group-concurrency N (execution.facts.backfill_group_concurrency; default 1) runs N
// documents of ONE ordered classification group at once in two phases — extraction and
// judge calls concurrent, assessments/votes/markers committed in event-time order — with
// the same final facts, assessments, resolutions and profiles as the serial run
// (memory/fact_backfill_staged.go). In-flight LLM calls are up to concurrency x N.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "facts-backfill:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		sourceType    = flag.String("source-type", "", "only documents with this documents.source_type")
		principal     = flag.String("principal", "", "only documents written by this principal (provenance tag or chunk source_agent_id)")
		namespace     = flag.String("namespace", "", "only documents whose writing principal id starts with this prefix (e.g. a LongMemEval run namespace)")
		tags          = flag.String("tags", "", "comma-separated classification tags every selected document must carry")
		limit         = flag.Int("limit", 0, "max documents (0 = all)")
		includeMarked = flag.Bool("include-marked", false, "also list documents with SOME marker; each is re-checked by content hash")
		dryRun        = flag.Bool("dry-run", false, "list and count only: no chunk text read, no LLM call")
		concurrency   = flag.Int("concurrency", 1, "documents (ordered: classification groups) in flight")
		docsPerMinute = flag.Float64("docs-per-minute", 0, "cap on documents started per minute (0 = no cap)")
		ordered       = flag.Bool("ordered", true, "run each classification group in event-time order on one worker")
		groupConc     = flag.Int("group-concurrency", 0, "documents of ONE ordered group in flight (two-phase: extraction and judge calls concurrent, commit in event-time order, same result as 1); 0 = execution.facts.backfill_group_concurrency, 1 = serial")
		progressEvery = flag.Int("progress-every", 25, "log progress every N documents")
	)
	flag.Parse()

	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgPath := os.Getenv("CAMBRIAN_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.json"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("config %s: %w", cfgPath, err)
	}
	if !cfg.Execution.Facts.ExtractEnabled {
		// The backfill IS extraction; the switch governs the ingest path. Everything
		// else (supersession, assessments, declared attributes) is read from config.
		slog.Info("facts-backfill: execution.facts.extract_enabled is off in config; enabled for this run")
		cfg.Execution.Facts.ExtractEnabled = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The adapter is the substrate's pool and the memory stack's raw tier; it is handed
	// to kernel.NewMemoryStack, which composes the enforcing tiers over it (AGENTS.md
	// §2.1). Nothing below reads or writes through it directly. An operator-run tool,
	// it serves no principal; ADR-0136 A1.7 is the backfill it runs (§2.2).
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pgvector: %w", err)
	}
	defer vec.Close()
	llmProvider, err := llm.NewProvider(cfg.LLMProvider, logger)
	if err != nil {
		return fmt.Errorf("llm provider: %w", err)
	}
	embedder := &llm.OllamaEmbedder{
		BaseURL:     cfg.Embedder.Endpoint,
		Model:       cfg.Embedder.Model,
		TimeoutMs:   cfg.Embedder.TimeoutMs,
		QueryPrefix: cfg.Embedder.QueryPrefix,
	}

	// The same registry declaration and substrate the kernel builds in app.go
	// (bootstrapKernel): core's memory_fact kind and its authority. Plugin kinds are
	// absent here, which is correct — this command writes and derives memory_fact only.
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		return fmt.Errorf("kind registry: %w", err)
	}
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), reg)
	assessor := llmProvider.Roles()[string(domain.PurposeFacts)]
	if assessor == "" {
		assessor = "default"
	}
	mem := kernel.NewMemoryStack(kernel.NewMemoryBackend(vec), llmProvider.GeneratorFor(domain.PurposeMemory), embedder, cfg.Execution,
		nil, cfg.Chunker, llmProvider.GeneratorFor(domain.PurposeFacts), kernel.FactSubstrate{
			Store:          ks,
			Reader:         postgres.NewPgFactItemReader(ks, "default", cfg.Embedder.Model, cfg.Embedder.Dimensions),
			Lister:         ks,
			EmbedModelID:   cfg.Embedder.Model,
			AssessorModel:  assessor,
			GeneratorModel: kernel.FactsGeneratorModel(cfg.LLMProvider, llmProvider.Roles()),
		}, kernel.RetrievalDeps{})
	if mem.FactBackfill == nil {
		return memory.ErrFactBackfillUnavailable
	}
	// Seeds the declared predicate profiles (supersession on) exactly as a kernel boot
	// does; its idle workers are drained by Stop.
	mem.FactExtractor.Start(ctx)
	defer mem.FactExtractor.Stop()

	// T-20261103-fact-backfill-processes-each-principal-s: the flag overrides config;
	// 0 (unset) reads execution.facts.backfill_group_concurrency (default 1 = serial).
	groupConcurrency := *groupConc
	if groupConcurrency <= 0 {
		groupConcurrency = max(1, cfg.Execution.Facts.BackfillGroupConcurrency)
	}

	var tagList []string
	for _, t := range strings.Split(*tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tagList = append(tagList, t)
		}
	}
	rep, err := mem.FactBackfill.Run(ctx, memory.FactBackfillOptions{
		Filter: domain.FactBackfillFilter{
			SourceType: *sourceType, Principal: *principal, Namespace: *namespace,
			Tags: tagList, Limit: *limit, IncludeMarked: *includeMarked,
		},
		DryRun:           *dryRun,
		Concurrency:      *concurrency,
		DocsPerMinute:    *docsPerMinute,
		Ordered:          *ordered,
		GroupConcurrency: groupConcurrency,
		ProgressEvery:    *progressEvery,
		Logger:           logger,
	})
	fmt.Printf("facts-backfill: extractor_version=%s candidates=%d already_done=%d pending=%d chunks=%d groups=%d ordered=%t concurrency=%d group_concurrency=%d max_llm_calls_per_document=%d estimated_llm_calls=%d\n",
		mem.FactExtractor.ExtractorVersion(), rep.Candidates, rep.AlreadyDone, rep.Pending, rep.Chunks, rep.Groups,
		*ordered, *concurrency, groupConcurrency, rep.MaxLLMCallsPerDocument, rep.EstimatedLLMCalls)
	if !*dryRun {
		fmt.Printf("facts-backfill: extracted=%d partial=%d skipped=%d failed=%d failed_parse=%d failed_timeout=%d parse_retries=%d parse_repaired=%d missing=%d cancelled=%d llm_calls=%d assessment_calls=%d assessments_replayed=%d facts_inserted=%d two_phase=%t journal_windows_replayed=%d assessment_calls_discarded=%d speculation_waves=%d elapsed=%s\n",
			rep.Extracted, rep.Partial, rep.Skipped, rep.Failed, rep.FailedParse, rep.FailedTimeout, rep.ParseRetries, rep.ParseRepaired, rep.Missing, rep.Cancelled, rep.LLMCalls, rep.AssessmentCalls,
			rep.AssessmentsReplayed, rep.FactsInserted, rep.TwoPhase, rep.JournalWindowsReplayed, rep.AssessmentCallsDiscarded,
			rep.SpeculationWaves, rep.Elapsed)
	}
	return err
}
