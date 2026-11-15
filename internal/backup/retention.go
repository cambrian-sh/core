package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Retention is the grandfather-father-son rotation (owner decision ENG-681: 7 daily /
// 4 weekly / 3 monthly). Days, ISO weeks and months are UTC, like the run names.
type Retention struct {
	Daily, Weekly, Monthly int
}

// rotated reports whether a run kind takes part in rotation. Snapshots taken before a
// destructive migration or a restore are the recovery point for a specific change and
// are kept until an operator removes them.
func rotated(kind string) bool { return kind == KindNightly || kind == KindManual }

// incompleteGrace is how long a run folder without a manifest is left alone (it may be
// a run in progress) before Prune removes it as a failed run.
const incompleteGrace = 24 * time.Hour

// Keep selects the runs to keep from runs (any order): for each tier, the newest
// complete rotated run of each of the most recent N distinct periods. Runs that do not
// rotate, and incomplete runs younger than incompleteGrace, are always kept.
func Keep(runs []RunInfo, r Retention, now time.Time) map[string]bool {
	keep := map[string]bool{}
	sorted := append([]RunInfo(nil), runs...)
	// newest first
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Created.After(sorted[j-1].Created); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	tiers := []struct {
		n   int
		key func(time.Time) string
	}{
		{r.Daily, func(t time.Time) string { return t.UTC().Format("2006-01-02") }},
		{r.Weekly, func(t time.Time) string { y, w := t.UTC().ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) }},
		{r.Monthly, func(t time.Time) string { return t.UTC().Format("2006-01") }},
	}
	for _, tier := range tiers {
		seen := map[string]bool{}
		for _, run := range sorted {
			if !run.Complete || !rotated(run.Kind) {
				continue
			}
			k := tier.key(run.Created)
			if seen[k] {
				continue
			}
			if len(seen) >= tier.n {
				break
			}
			seen[k] = true
			keep[run.Name] = true
		}
	}
	for _, run := range sorted {
		if run.Complete && !rotated(run.Kind) {
			keep[run.Name] = true
		}
		if !run.Complete && now.Sub(run.Created) < incompleteGrace {
			keep[run.Name] = true
		}
	}
	return keep
}

// Prune applies r to the runs under root and removes the rest, together with their
// encrypted secret.key folders under keyRoot. Only folders whose names are run names are
// ever considered. It returns the removed folder names.
func Prune(root, keyRoot string, r Retention, now time.Time) ([]string, error) {
	runs, err := ListRuns(root)
	if err != nil {
		return nil, err
	}
	keep := Keep(runs, r, now)
	var removed []string
	for _, run := range runs {
		if keep[run.Name] {
			continue
		}
		if err := os.RemoveAll(run.Dir); err != nil {
			return removed, fmt.Errorf("prune %s: %w", run.Dir, err)
		}
		if keyRoot != "" {
			if err := os.RemoveAll(filepath.Join(keyRoot, run.Name)); err != nil {
				return removed, fmt.Errorf("prune key folder %s: %w", run.Name, err)
			}
		}
		removed = append(removed, run.Name)
	}
	// Key folders whose run is gone (removed by hand) follow their run.
	if keyRoot != "" {
		keys, err := ListRuns(keyRoot)
		if err != nil {
			return removed, err
		}
		present := map[string]bool{}
		for _, run := range runs {
			present[run.Name] = true
		}
		for _, k := range keys {
			if !present[k.Name] && now.Sub(k.Created) >= incompleteGrace {
				if err := os.RemoveAll(k.Dir); err != nil {
					return removed, fmt.Errorf("prune orphan key folder %s: %w", k.Name, err)
				}
			}
		}
	}
	return removed, nil
}
