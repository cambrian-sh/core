// config-migrate rewrites a config file from the v1 flat `execution` schema to
// the v2 nested one, in place.
//
// Loading already migrates in memory, so this is never required — it exists so an
// operator can move their file to the current schema deliberately, review the
// diff, and stop relying on the compatibility path. Running it on a v2 file is a
// no-op.
//
//	go run ./cmd/config-migrate configs/tuning.json [more.json...]
package main

import (
	"fmt"
	"os"

	"github.com/cambrian-sh/core/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: config-migrate <config.json> [...]")
		os.Exit(2)
	}
	rc := 0
	for _, path := range os.Args[1:] {
		moved, err := config.MigrateFileInPlace(path)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "FAIL  %s: %v\n", path, err)
			rc = 1
		case len(moved) == 0:
			fmt.Printf("OK    %s (already schema v%d)\n", path, config.CurrentSchemaVersion)
		default:
			fmt.Printf("MOVED %s (%d keys → nested blocks, now schema v%d)\n",
				path, len(moved), config.CurrentSchemaVersion)
		}
	}
	os.Exit(rc)
}
