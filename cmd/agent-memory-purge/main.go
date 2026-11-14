// agent-memory-purge deletes the agent-experience rows the kernel used to write
// straight through MemoryManager.Save, past the ingestion pipeline: raw tool outputs,
// step results and failure records.
//
// Owner decision 2026-11-14 (ENG-1595, T-20261114-rework-agent-experience-memory-raw-step-results):
// "No bypass ... just saving the step result, tool outputs etc is completely shit."
// The writers were stopped in the same change (internal/memory/agent.go); this tool
// removes what they already stored. It is an explicit, logged, OWNER-RUN step — never a
// boot-time migration — so a deployment loses nothing until someone decides to run it.
//
// WHAT IS DELETED (purgeClasses), all from `chunks`:
//   - document_type 'mnemonic_action': mutation tool-call lines (ADR-0049 D1 action records);
//   - document_type 'negative_edge': failure records carrying a failed plan's output (A2.3)
//     and the older unparented error reports;
//   - document_type 'mnemonic_fact' rows the deleted ADR-0015 raw path wrote: those the
//     tier-2 judge scored (scoring_prompt_version <> ”) and the tier-1 tool-output rows
//     (metadata.source_agent = 'ToolOutput'). Ingested document chunks are also
//     'mnemonic_fact' but carry neither marker, so they are never selected.
//
// With each selected chunk go, in one transaction: its document_edges (either endpoint;
// that table has no FK on target_id), chunk_triplets and chunk_pagerank rows (no FK at
// all), and — by ON DELETE CASCADE — its chunk_embeddings and experience_derivations rows.
// The ADR-0095 `experiences` episode rows stay: they carry no step output.
//
// Dry-run by default: prints the count per class and touches nothing. --apply deletes.
//
// Usage:
//
//	agent-memory-purge            # report what would be deleted
//	agent-memory-purge --apply    # delete it
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// purgeClass is one kind of raw agent-experience row: a name for the report and the
// SQL predicate over `chunks c` that selects it.
type purgeClass struct {
	name  string
	where string
}

var purgeClasses = []purgeClass{
	{"mnemonic_action (tool-call action records)", `c.document_type = 'mnemonic_action'`},
	{"negative_edge (failure records)", `c.document_type = 'negative_edge'`},
	{"mnemonic_fact scored by the deleted tier-2 judge", `c.document_type = 'mnemonic_fact' AND c.scoring_prompt_version <> ''`},
	{"mnemonic_fact tier-1 tool outputs", `c.document_type = 'mnemonic_fact' AND c.scoring_prompt_version = '' AND c.metadata->>'source_agent' = 'ToolOutput'`},
}

// selectedChunks is the union predicate, used by every DELETE so the statements can
// never disagree about which chunks are purged.
func selectedChunks() string {
	s := ""
	for i, c := range purgeClasses {
		if i > 0 {
			s += " OR "
		}
		s += "(" + c.where + ")"
	}
	return s
}

// deleteStatements are executed in order inside one transaction: dependents without a
// foreign key first, then the chunks (whose embeddings and derivations cascade).
func deleteStatements() []string {
	sel := "SELECT c.id FROM chunks c WHERE " + selectedChunks()
	return []string{
		"DELETE FROM document_edges WHERE source_id IN (" + sel + ") OR target_id IN (" + sel + ")",
		"DELETE FROM chunk_triplets WHERE chunk_id IN (" + sel + ")",
		"DELETE FROM chunk_pagerank WHERE chunk_id IN (" + sel + ")",
		"DELETE FROM chunks c WHERE " + selectedChunks(),
	}
}

func main() {
	apply := flag.Bool("apply", false, "delete the rows; omitted, the tool only reports counts")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := config.LoadDotEnv(".env"); err != nil {
		slog.Error("load .env failed", "err", err)
		os.Exit(1)
	}
	cfgPath := os.Getenv("CAMBRIAN_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.json"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	ctx := context.Background()
	// Raw PgVectorAdapter pool (tier 0, AGENTS.md §2.2 "kernel-authored artifact
	// writer"): an operator-run maintenance tool deleting kernel-authored rows the owner
	// ordered removed (ENG-1595). It serves no principal and is not part of the kernel's
	// object graph, so MemoryStack (§2.1) does not apply; AGENTS.md §6 gap 1.
	pg, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		slog.Error("pgvector connect failed", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	pool := pg.Pool()

	total := 0
	for _, c := range purgeClasses {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks c WHERE "+c.where).Scan(&n); err != nil {
			slog.Error("count failed", "class", c.name, "err", err)
			os.Exit(1)
		}
		fmt.Printf("%-55s %d\n", c.name, n)
		total += n
	}
	fmt.Printf("%-55s %d\n", "TOTAL chunks", total)

	if !*apply {
		fmt.Println("\nDRY RUN — nothing deleted. Re-run with --apply to delete these rows.")
		return
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, stmt := range deleteStatements() {
			tag, err := tx.Exec(ctx, stmt)
			if err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
			slog.Info("purged", "statement", stmt[:min(len(stmt), 40)], "rows", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		slog.Error("purge failed; nothing deleted (rolled back)", "err", err)
		os.Exit(1)
	}
	slog.Info("agent-experience purge complete", "chunks", total)
}
