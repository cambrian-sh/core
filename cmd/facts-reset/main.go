// facts-reset is the HARD RESET of the ADR-0136 fact layer for a re-run (Amendment
// A2-Q3; owner answers A2-Q2/Q3, 2026-11-03). It deletes, for the memory_fact items
// asserted by the selected principals, the items (their statement values and item
// embeddings cascade), the resolution versions on their keys, the assessments that cite
// them and the memory_fact extraction markers of the documents those principals wrote —
// and, unless -keep-learned, every learned memory_fact predicate profile (profiles are
// namespace-wide, so votes learned from the reset subset would otherwise survive it).
// Surviving keys whose inputs changed are re-derived. A following cmd/facts-backfill
// then extracts the selection again from scratch.
//
// It is NOT ADR-0137 erasure: erasure leaves content-free sentinels so a closed chain
// stays closed; this removes the facts outright, as if extraction had never run. Use it
// for experiments (a changed prompt or backbone, a fresh arm), with no ingest writing
// into the selection while it runs.
//
// It composes through the kernel builder (kernel.NewMemoryStack → MemoryStack.FactReset,
// postgres.PgKnowledgeStore.ResetFacts), one transaction; the command itself holds no
// SQL. -dry-run executes the same statements and rolls back: exact counts, no change.
//
// Usage (from cambrian-core/, CAMBRIAN_CONFIG selects the config file):
//
//	go run ./cmd/facts-reset -dry-run -namespace lme-run1-
//	go run ./cmd/facts-reset -namespace lme-run1-
//	go run ./cmd/facts-reset -principal lme-run1-q42 -keep-learned
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "facts-reset:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		namespace   = flag.String("namespace", "", "reset facts whose writing principal id starts with this prefix (e.g. a LongMemEval run namespace)")
		principal   = flag.String("principal", "", "reset facts of exactly this principal")
		all         = flag.Bool("all", false, "reset EVERY memory_fact item (required when neither -namespace nor -principal is set)")
		dryRun      = flag.Bool("dry-run", false, "count what would be deleted; change nothing")
		keepLearned = flag.Bool("keep-learned", false, "keep the learned memory_fact predicate profiles (default: delete them)")
	)
	flag.Parse()
	if *namespace == "" && *principal == "" && !*all {
		return errors.New("no selector: pass -namespace, -principal, or -all")
	}

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The adapter is the substrate's pool and the memory stack's raw tier; it is handed
	// to kernel.NewMemoryStack (AGENTS.md §2.1). Nothing below reads or writes through it.
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pgvector: %w", err)
	}
	defer vec.Close()
	// The same registry declaration the kernel builds (bootstrapKernel): re-derivation
	// after the reset runs core's valid_time_supersession authority.
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		return fmt.Errorf("kind registry: %w", err)
	}
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), reg)
	// No generator and no embedder: a reset makes no model call. Extraction stays off
	// for this stack (cfg.Execution as loaded), so no extractor or queue is built.
	mem := kernel.NewMemoryStack(vec, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, kernel.FactSubstrate{
		Store:    ks,
		Reader:   postgres.NewPgFactItemReader(ks, "default", cfg.Embedder.Model, cfg.Embedder.Dimensions),
		Resetter: ks,
	})
	if mem.FactReset == nil {
		return errors.New("no fact reset in this memory stack")
	}
	rep, err := mem.FactReset.ResetFacts(ctx, domain.FactResetFilter{
		Namespace: *namespace, Principal: *principal, All: *all, KeepLearned: *keepLearned, DryRun: *dryRun,
	})
	mode := "deleted"
	if *dryRun {
		mode = "would delete (dry run, nothing changed)"
	}
	fmt.Printf("facts-reset: %s items=%d resolutions=%d assessments=%d markers=%d learned_profiles=%d rederived_keys=%d\n",
		mode, rep.Items, rep.Resolutions, rep.Assessments, rep.Markers, rep.Profiles, rep.Rederived)
	return err
}
