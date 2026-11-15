package domain

import (
	"context"
	"testing"
	"time"
)

// The ingest origin is a context value: it survives WithoutCancel (the async fact job's
// ctx), an earlier origin a door stamped is kept, and a zero time is ignored.
func TestIngestOrigin_SurvivesWithoutCancelAndKeepsTheEarliestStamp(t *testing.T) {
	if _, ok := IngestOriginFrom(context.Background()); ok {
		t.Fatal("no origin on a bare context")
	}
	if _, ok := IngestOriginFrom(WithIngestOrigin(context.Background(), time.Time{})); ok {
		t.Fatal("a zero origin must be ignored")
	}
	early := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ctx := EnsureIngestOrigin(WithIngestOrigin(context.Background(), early))
	if got, _ := IngestOriginFrom(context.WithoutCancel(ctx)); !got.Equal(early) {
		t.Fatalf("origin = %v, want the door's earlier stamp", got)
	}
	before := time.Now()
	got, ok := IngestOriginFrom(EnsureIngestOrigin(context.Background()))
	if !ok || got.Before(before.Add(-time.Second)) {
		t.Fatalf("EnsureIngestOrigin must stamp now, got %v", got)
	}
}

func TestErasureCandidate_IsTaggable(t *testing.T) {
	c := ErasureCandidate{Kind: ErasureTargetChunk, ID: "c1", Tags: []string{"x"}}
	if c.AuthzRef().ID != "chunk:c1" || c.AuthzRef().Kind != KindMemory || len(c.AuthzTags()) != 1 {
		t.Fatalf("taggable = %+v %v", c.AuthzRef(), c.AuthzTags())
	}
}
