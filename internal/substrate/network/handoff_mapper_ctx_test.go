package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ENG-2055: the TelemetryObserver callbacks take the caller's ctx, so the schema
// mismatch metric records under the stream's ctx (its trace), not Background.

type mismatchKey struct{}

type ctxObserver struct {
	domain.NoopTelemetryObserver
	got  context.Context
	kind string
}

func (o *ctxObserver) OnSchemaMismatch(ctx context.Context, _, kind string) {
	o.got, o.kind = ctx, kind
}

func TestProtoToHandoff_SchemaMismatchCarriesTheCallerContext(t *testing.T) {
	ctx := context.WithValue(t.Context(), mismatchKey{}, "stream")
	obs := &ctxObserver{}
	protoToHandoff(ctx, &pb.Handoff{ToAgent: "a"}, obs) // no Id/FromAgent: a mismatch
	if obs.kind != "missing_required_field" {
		t.Fatalf("mismatch kind = %q", obs.kind)
	}
	if obs.got == nil || obs.got.Value(mismatchKey{}) != "stream" {
		t.Fatal("OnSchemaMismatch must receive the caller's ctx")
	}
}
