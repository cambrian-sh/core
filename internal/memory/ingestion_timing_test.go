package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// agentTimedParser reports a sidecar parse time like the real docling_agent and
// records it the way the dispatcher does.
type agentTimedParser struct{}

func (agentTimedParser) Parse(ctx context.Context, req StructureParseRequest) (*StructuredDocument, error) {
	domain.StageClockFrom(ctx).Add(StageStructureAgent, 2*time.Millisecond)
	return nil, nil // flat-chunk fallback
}

// T-20261102-ingestion-logs-no-per-stage-timing-so-the-1-s: a sync ingest records
// every synchronous stage it ran on the clock the caller put on ctx, so the gRPC
// handler can return the breakdown and the log line can attribute the cost.
func TestProcessSync_RecordsIngestStages(t *testing.T) {
	t.Parallel()
	im := newCaptureIngestManager(t, agentTimedParser{})
	clock := domain.NewStageClock()
	ctx := domain.WithStageClock(context.Background(), clock)

	doc := domain.ExternalDocument{
		SourceURI: "ingest_memory://s1", SourceType: "ingest_memory", ThreadID: "s1",
		Title: "session one", Body: "user: I adopted a cat named Luna.\nassistant: Congratulations on Luna!",
	}
	if _, err := im.ProcessSync(ctx, doc); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	got := map[string]bool{}
	for _, s := range clock.Stages() {
		got[s.Stage] = true
	}
	for _, want := range []string{StageChunk, StageStructureParse, StageStructureAgent, StageStructureLeaves,
		StagePrepare, StageEmbed, StageChunkSave, StageEntity, StagePublish} {
		if !got[want] {
			t.Errorf("stage %q not recorded; got %v", want, clock.Stages())
		}
	}
	counts := clock.Counts()
	if counts[CountEmbedCalls] != 1 || counts[CountChunks] < 1 {
		t.Errorf("counts: want 1 embed call and >=1 chunk, got %v", counts)
	}

	line := FormatStageClock(clock, 5*time.Millisecond)
	if !strings.HasPrefix(line, "total=5.0,") || !strings.Contains(line, "embed=") || !strings.Contains(line, "embed_calls#=1") {
		t.Errorf("FormatStageClock: %q", line)
	}
}

// A context without a clock costs nothing and records nothing: the nil clock is
// a no-op, never a panic.
func TestStageClock_NilSafe(t *testing.T) {
	t.Parallel()
	var c *domain.StageClock
	c.Add("x", time.Second)
	c.Count("y", 1)
	c.Since("z", time.Now())
	if c.Stages() != nil || c.Counts() != nil {
		t.Fatal("nil clock must report nothing")
	}
	if domain.StageClockFrom(context.Background()) != nil {
		t.Fatal("no clock on a bare context")
	}
}
