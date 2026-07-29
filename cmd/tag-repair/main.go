// tag-repair removes IDENTITY tags from a document's CLASSIFICATION.
//
// Two different things have been travelling in `documents.tags`:
//
//   - a classification — "what this document is", drawn from the controlled
//     vocabulary, and the only thing access policy can act on (ADR-0085);
//   - an identity — the `source_document` + `<id>` pair that
//     `externalDocumentID` reads to give a document a stable, unique id.
//
// The second is not a label. Access policy acts on labels and never on a
// document by name — restricting one named document is an explicit non-goal —
// so a tag that names exactly one document is a term no rule will ever
// usefully match. Left in place they swamp the vocabulary: measured on the live
// store, 726 distinct tags for a 12-term vocabulary, 710 of them a document's
// own id. A picker over that is unusable.
//
// The repair is deliberately narrow. It removes only:
//
//  1. a tag equal to the document's own id, and
//  2. the `source_document` marker that introduces it.
//
// Everything else is left exactly as it is. A tag shared by many documents is a
// real classification however unofficial it looks, and this tool does not get to
// decide that a term outside the current vocabulary is worthless — vocabularies
// change, and silently deleting labels an operator may have written by hand is
// not a repair.
//
// Writes go through PgVectorAdapter.RetagDocument, never raw SQL: the document
// row is authoritative and the per-chunk `metadata.tags` copies are a derived
// cache (ADR-0093). Only that method moves both in one transaction, so a
// document can never be left half-labelled. An UPDATE against `documents` alone
// would leave every chunk still carrying the junk and still matching it under
// the retrieval-path GIN filter.
//
// Dry-run by default; --apply writes.
//
// Usage:
//
//	tag-repair                 # report what would change
//	tag-repair --apply         # perform the repair
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// identityMarker is the tag the ingest path uses to announce "the next tag is
// this document's id" (see externalDocumentID rule 1).
const identityMarker = "source_document"

type docRow struct {
	id   string
	tags []string
}

func main() {
	var (
		apply = flag.Bool("apply", false, "write the repair; omitted, the tool only reports")
		limit = flag.Int("limit", 0, "max documents to repair (0 = all)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

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
	pg, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		slog.Error("pgvector connect failed", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	rows, err := loadDocuments(ctx, pg)
	if err != nil {
		slog.Error("scan documents failed", "err", err)
		os.Exit(1)
	}

	before := distinctTags(rows)

	var pending []docRow
	for _, r := range rows {
		cleaned := classification(r.id, r.tags)
		if len(cleaned) != len(r.tags) {
			pending = append(pending, docRow{id: r.id, tags: cleaned})
		}
	}

	after := map[string]struct{}{}
	for _, r := range rows {
		for _, t := range classification(r.id, r.tags) {
			after[t] = struct{}{}
		}
	}

	slog.Info("scan complete",
		"documents", len(rows),
		"documents_to_repair", len(pending),
		"distinct_tags_before", len(before),
		"distinct_tags_after", len(after))

	if !*apply {
		fmt.Println("\nDRY RUN — nothing written. Re-run with --apply to perform the repair.")
		printSurvivors(after)
		return
	}

	if *limit > 0 && len(pending) > *limit {
		pending = pending[:*limit]
	}

	repaired, failed := 0, 0
	for _, r := range pending {
		if err := pg.RetagDocument(ctx, r.id, r.tags); err != nil {
			// Reported per document rather than aborting: a repair that stops
			// halfway is still a correct store (each document moves atomically),
			// and the count below has to be honest about what actually landed.
			slog.Error("retag failed", "doc", r.id, "err", err)
			failed++
			continue
		}
		repaired++
	}

	slog.Info("repair complete", "repaired", repaired, "failed", failed)
	printSurvivors(after)
	if failed > 0 {
		os.Exit(1)
	}
}

// classification strips the identity pair from a tag list, preserving the order
// and every other term.
func classification(docID string, tags []string) []string {
	out := make([]string, 0, len(tags))
	for i := 0; i < len(tags); i++ {
		t := tags[i]
		// The marker plus the id it introduces. Checking the NEXT tag against the
		// document id — rather than dropping whatever follows the marker — means a
		// document that merely happens to carry the marker keeps its real labels.
		if t == identityMarker {
			if i+1 < len(tags) && tags[i+1] == docID {
				i++ // consume the id too
			}
			continue
		}
		if t == docID {
			continue
		}
		out = append(out, t)
	}
	return out
}

func loadDocuments(ctx context.Context, pg *postgres.PgVectorAdapter) ([]docRow, error) {
	rows, err := pg.Pool().Query(ctx, `SELECT id, tags FROM `+postgres.TableDocuments+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []docRow
	for rows.Next() {
		var r docRow
		if err := rows.Scan(&r.id, &r.tags); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func distinctTags(rows []docRow) map[string]struct{} {
	seen := map[string]struct{}{}
	for _, r := range rows {
		for _, t := range r.tags {
			seen[t] = struct{}{}
		}
	}
	return seen
}

func printSurvivors(after map[string]struct{}) {
	tags := make([]string, 0, len(after))
	for t := range after {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	fmt.Printf("\nClassification vocabulary in use after repair (%d):\n", len(tags))
	for _, t := range tags {
		fmt.Printf("  %s\n", t)
	}
}
