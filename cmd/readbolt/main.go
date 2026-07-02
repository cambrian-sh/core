package main

import (
	"encoding/json"
	"fmt"
	"os"

	"go.etcd.io/bbolt"
)

func main() {
	db, err := bbolt.Open("data/agents.db", 0400, &bbolt.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
		bk := tx.Bucket([]byte("agents"))
		if bk == nil {
			return fmt.Errorf("no agents bucket")
		}
		c := bk.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			ks := string(k)
			if ks != "kg_extractor_agent" && ks != "scout_agent" && ks != "reranker_agent" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(v, &rec); err != nil {
				fmt.Println(ks, "json-err:", err)
				continue
			}
			fmt.Printf("--- %s ---\n", ks)
			for _, kk := range []string{"id", "exec_path", "dir", "runtime"} {
				fmt.Printf("  %s = %q\n", kk, fmt.Sprint(rec[kk]))
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintln(os.Stderr, "view:", err)
		os.Exit(1)
	}
}
