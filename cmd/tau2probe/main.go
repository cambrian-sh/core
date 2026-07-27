package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tau2probe answers one question: did the tau2-knowledge ingest actually land rows that
// retrieval could return, and do they carry the session_id the suite matches on?
func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Args[1])
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	q := func(label, sql string) {
		var n int
		if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			fmt.Printf("%-46s ERR %v\n", label, err)
			return
		}
		fmt.Printf("%-46s %d\n", label, n)
	}

	q("chunks total", "SELECT count(*) FROM chunks")
	q("chunks with any metadata session_id", "SELECT count(*) FROM chunks WHERE metadata ? 'session_id'")
	q("chunks with tau2k session_id", "SELECT count(*) FROM chunks WHERE metadata->>'session_id' LIKE 'tau2k%'")
	q("chunks with embedding", "SELECT count(*) FROM chunks WHERE embedding IS NOT NULL")
	q("chunks tau2k WITH embedding",
		"SELECT count(*) FROM chunks WHERE metadata->>'session_id' LIKE 'tau2k%' AND embedding IS NOT NULL")
	q("documents total", "SELECT count(*) FROM documents")
	q("LEGACY rows with session_id", "SELECT count(*) FROM documents_legacy WHERE metadata ? 'session_id'")
	q("LEGACY rows with tags", "SELECT count(*) FROM documents_legacy WHERE metadata ? 'tags'")
	q("chunks with tags", "SELECT count(*) FROM chunks WHERE metadata ? 'tags'")
	q("chunks with document_id meta", "SELECT count(*) FROM chunks WHERE metadata ? 'document_id'")
	q("documents whose id starts doc", "SELECT count(*) FROM documents WHERE id LIKE 'doc%'")
	q("chunks parented to those docs", "SELECT count(*) FROM chunks c JOIN documents d ON c.document_id=d.id WHERE d.id LIKE 'doc%'")
	q("...of those WITH embedding", "SELECT count(*) FROM chunks c JOIN documents d ON c.document_id=d.id WHERE d.id LIKE 'doc%' AND c.embedding IS NOT NULL")
	q("chunks with ingest_thread_id", "SELECT count(*) FROM chunks WHERE metadata ? 'ingest_thread_id'")
	q("chunks ingest_thread_id LIKE tau2k", "SELECT count(*) FROM chunks WHERE metadata->>'ingest_thread_id' LIKE 'tau2k%'")
	q("chunks meta document_id starts doc", "SELECT count(*) FROM chunks WHERE metadata->>'document_id' LIKE 'doc%'")

	rs, e := pool.Query(ctx, `
		SELECT DISTINCT metadata->>'document_id', metadata->'tags'
		FROM chunks WHERE metadata->>'document_id' LIKE 'doc%' LIMIT 3`)
	if e == nil {
		fmt.Println("--- sample document_id + tags ---")
		for rs.Next() {
			var did string
			var tags any
			if err := rs.Scan(&did, &tags); err == nil {
				fmt.Println("  document_id=" + did + "  tags=" + fmt.Sprint(tags))
			}
		}
		rs.Close()
	}

	// What session_id prefixes exist at all?
	rows, err := pool.Query(ctx, `
		SELECT split_part(metadata->>'session_id', ':', 1) AS pfx, count(*)
		FROM chunks WHERE metadata ? 'session_id'
		GROUP BY 1 ORDER BY 2 DESC LIMIT 8`)
	if err == nil {
		fmt.Println("--- session_id prefixes in chunks ---")
		for rows.Next() {
			var p string
			var n int
			if err := rows.Scan(&p, &n); err == nil {
				fmt.Printf("  %-40s %d\n", p, n)
			}
		}
		rows.Close()
	}

	// And a sample of the most recent chunk metadata keys.
	var meta string
	if err := pool.QueryRow(ctx,
		`SELECT metadata::text FROM chunks ORDER BY created_at DESC LIMIT 1`).Scan(&meta); err == nil {
		if len(meta) > 400 {
			meta = meta[:400]
		}
		fmt.Println("--- newest chunk metadata ---")
		fmt.Println(" ", meta)
	}
}
