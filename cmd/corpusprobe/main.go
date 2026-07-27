package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// corpusprobe reports the row distribution across the pre/post ADR-0093 tables, so a
// migration of the live corpus can be checked rather than trusted.
func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Args[1])
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	for _, t := range []string{"documents", "documents_legacy", "chunks", "document_sections", "tools", "skills", "agent_profiles"} {
		var n int
		err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", t)).Scan(&n)
		if err != nil {
			fmt.Printf("%-20s absent\n", t)
			continue
		}
		fmt.Printf("%-20s %d\n", t, n)
	}

	fmt.Println("--- by document_type ---")
	for _, t := range []string{"documents", "documents_legacy", "chunks"} {
		rows, err := pool.Query(ctx,
			fmt.Sprintf("SELECT document_type, count(*) FROM %s GROUP BY 1 ORDER BY 2 DESC", t))
		if err != nil {
			continue
		}
		fmt.Println("[" + t + "]")
		for rows.Next() {
			var dt string
			var n int
			if err := rows.Scan(&dt, &n); err == nil {
				fmt.Printf("  %-24s %d\n", dt, n)
			}
		}
		rows.Close()
	}

	rows2, e2 := pool.Query(ctx, "SELECT id, name, mode, rule FROM authz_policies")
	if e2 == nil {
		fmt.Println("--- authz_policies ---")
		for rows2.Next() {
			var id, name, mode, rule string
			if err := rows2.Scan(&id, &name, &mode, &rule); err == nil {
				fmt.Println("  " + id + " | " + name + " | " + mode + " | " + rule)
			}
		}
		rows2.Close()
	} else {
		fmt.Println("authz_policies:", e2)
	}

	rows3, e3 := pool.Query(ctx, "SELECT policy_id, container_kind, target_id FROM authz_links")
	if e3 == nil {
		fmt.Println("--- authz_links ---")
		for rows3.Next() {
			var pid, kind, target string
			if err := rows3.Scan(&pid, &kind, &target); err == nil {
				fmt.Println("  " + pid + " -> " + kind + ":" + target)
			}
		}
		rows3.Close()
	}

	rows4, e4 := pool.Query(ctx, `
		SELECT tc.table_name, tc.constraint_name, ccu.table_name AS refs
		FROM information_schema.table_constraints tc
		JOIN information_schema.constraint_column_usage ccu ON tc.constraint_name = ccu.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND ccu.table_name IN ('documents','documents_legacy')`)
	if e4 == nil {
		fmt.Println("--- FKs referencing documents/documents_legacy ---")
		for rows4.Next() {
			var t, c, refs string
			if err := rows4.Scan(&t, &c, &refs); err == nil {
				fmt.Println("  " + t + "." + c + " -> " + refs)
			}
		}
		rows4.Close()
	}

	// Parentage coverage: how many chunks resolved a document.
	var parented, orphan int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE document_id IS NOT NULL").Scan(&parented)
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE document_id IS NULL").Scan(&orphan)
	if parented+orphan > 0 {
		fmt.Printf("--- chunks parented=%d orphan=%d ---\n", parented, orphan)
	}
}
