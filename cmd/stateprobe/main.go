package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// stateprobe reports the containers and labels an operator can actually reference,
// which is exactly what the policy assistant needs to be told and currently is not.
func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Args[1])
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	list := func(label, sql string, cols int) {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			fmt.Printf("%s: %v\n", label, err)
			return
		}
		defer rows.Close()
		fmt.Println("--- " + label + " ---")
		n := 0
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				continue
			}
			out := "  "
			for i, v := range vals {
				if i >= cols {
					break
				}
				out += fmt.Sprint(v) + "  "
			}
			fmt.Println(out)
			n++
		}
		if n == 0 {
			fmt.Println("  (none)")
		}
	}

	list("registered ingresses (surfaces)", "SELECT agent_id, surface_kind, surface_id FROM authz_ingress", 3)
	list("groups", "SELECT id, name FROM authz_groups", 2)
	list("policies", "SELECT id, mode FROM authz_policies", 2)
	list("documents matching 'armata'",
		"SELECT id, tags FROM documents WHERE lower(id) LIKE '%armata%' LIMIT 5", 2)
	list("armata chunks (searchable?)",
		"SELECT id, (embedding IS NOT NULL) FROM chunks WHERE document_id LIKE '%armata%' LIMIT 5", 2)
	list("armata chunk count",
		"SELECT count(*), sum(CASE WHEN embedding IS NOT NULL THEN 1 ELSE 0 END) FROM chunks WHERE document_id LIKE '%armata%'", 2)
	list("tag pollution: distinct tags vs vocabulary size",
		"SELECT count(DISTINCT t) FROM (SELECT unnest(tags) AS t FROM documents) x", 1)
	list("docs with zero labels",
		"SELECT count(*) FROM documents WHERE cardinality(tags) = 0", 1)
	list("total documents", "SELECT count(*) FROM documents", 1)
}
