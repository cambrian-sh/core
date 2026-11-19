package objects_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1103 (owner decision 2026-11-19): the observation write takes the
// entity's object lock INSIDE its INSERT (objects.LockedInsertCTE) and checks
// the declarations after it. Race real writers (PgEventStore.RecordObservation
// on the live ontology) against a live ontology Apply that turns the kind into
// a table, many times: no observation may ever be missing from the object
// table — whether it committed before the Apply's rebuild (the rebuild reads
// it) or after (its own write sees the table).
func TestPg_ObservationWritersRacingATableApplyLoseNothing(t *testing.T) {
	const iterations, writers, perWriter = 12, 4, 15
	kinds := make([]domain.KindSpec, iterations)
	names := make([]string, iterations)
	for i := range kinds {
		names[i] = fmt.Sprintf("p3race%02d", i)
		kinds[i] = domain.KindSpec{Kind: names[i], Open: true, Predicates: map[string]domain.ValueSpec{"carrier": {Type: "text"}}}
	}
	e := newEnv(t, "p3test-race", names, kinds...)
	ev := e.evidence("p3race-ev", "wms", "ops")

	for i, spec := range kinds {
		var wg sync.WaitGroup
		errs := make(chan error, writers*perWriter+1)
		start := make(chan struct{})
		want := map[string]string{}
		var mu sync.Mutex
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for k := 0; k < perWriter; k++ {
					entity := fmt.Sprintf("%s/E-%d-%d", spec.Kind, w, k)
					val := fmt.Sprintf("c-%d-%d", w, k)
					ref := fmt.Sprintf("race:%d:%d:%d", i, w, k)
					if err := e.observe(entity, "carrier", txt(val), time.Duration(k)*time.Minute, ev, ref); err != nil {
						errs <- fmt.Errorf("%s: %w", entity, err)
						return
					}
					mu.Lock()
					want[entity] = val
					mu.Unlock()
				}
			}(w)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			table := spec
			table.Table = true
			res, err := e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{table}}, "user:ops", false)
			if err != nil || !res.Applied {
				errs <- fmt.Errorf("apply %s: %+v %w", spec.Kind, res, err)
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		rows := e.rows(spec.Kind)
		for entity, val := range want {
			r := rows[entity]
			if r == nil {
				t.Fatalf("iteration %d: observation of %s is missing from %s (a write lost to the Apply race)", i, entity, spec.Kind)
			}
			if r["carrier__text"] != val {
				t.Fatalf("iteration %d: %s carrier = %v, want %s", i, entity, r["carrier__text"], val)
			}
		}
		if len(rows) != len(want) {
			t.Fatalf("iteration %d: %d rows for %d written entities", i, len(rows), len(want))
		}
	}
}
