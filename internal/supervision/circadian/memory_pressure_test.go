package circadian_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/supervision/circadian"
)

// Cycle — MemoryLifecycleManager subscribes to MemoryPressureEvent and
// triggers global consolidation when the event is published.
func TestMLM_ReactsToMemoryPressureEvent(t *testing.T) {
	store := &stubSessionStore{}
	var globalConsolidationCalled int32
	cons := &pressureConsolidator{
		onGlobal: func() { atomic.AddInt32(&globalConsolidationCalled, 1) },
	}
	bus := newStubBus()

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 7*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mlm.Start(ctx)

	_ = bus.Publish(domain.MemoryPressureEvent{
		TotalDocuments: 5000,
		IndexSizeBytes: 1024 * 1024,
		Trigger:        string(domain.ConsolidationTriggerPressure),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&globalConsolidationCalled) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&globalConsolidationCalled) < 1 {
		t.Error("expected global consolidation to run after MemoryPressureEvent")
	}
}

type pressureConsolidator struct {
	onGlobal func()
}

func (p *pressureConsolidator) Consolidate(ctx context.Context, sess domain.Session) error {
	if sess.ID == "" && p.onGlobal != nil {
		p.onGlobal()
	}
	return nil
}
