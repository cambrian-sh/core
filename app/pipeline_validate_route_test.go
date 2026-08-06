package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The validate route across pipeline authors (contract 0091/0092).
//
// The defect this pins shipped twice: preferring a source by "FieldsJSON is
// non-empty" cannot work, because an author WITHOUT the trigger schema still
// returns a full projection — every availability a named unknown — which is
// non-empty JSON. The editor's field picker then showed "no schema source" for
// an ingress pipeline whenever the schema-less plugin registered first.

type fakeAuthor struct {
	fields   string
	resolved bool
}

func (f *fakeAuthor) GetPipeline(context.Context, string, int) (domain.PipelineGraph, error) {
	return domain.PipelineGraph{}, domain.ErrPipelineNotFound
}

func (f *fakeAuthor) ValidatePipeline(context.Context, string) (domain.PipelineValidation, error) {
	return domain.PipelineValidation{
		NodeCount:      3,
		FieldsJSON:     f.fields,
		FieldsResolved: f.resolved,
	}, nil
}

func TestValidateRoute_PrefersTheAuthorHoldingTheTriggerSchema(t *testing.T) {
	// Registration order is the trap: the schema-less author first.
	unknowns := `{"gate":{"unknown":"no schema source exists for a ingress trigger"}}`
	known := `{"gate":{"available":[{"path":"properties.mag","types":["number"]}]}}`

	holder := &pipelineSourceSet[domain.PipelineAuthor]{}
	holder.add(&fakeAuthor{fields: unknowns, resolved: false})
	holder.add(&fakeAuthor{fields: known, resolved: true})

	got, err := deferredPipelineAuthor{holder: holder}.ValidatePipeline(context.Background(), "{}")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !got.FieldsResolved || got.FieldsJSON != known {
		t.Fatalf("the resolved projection must win regardless of registration order, got resolved=%v json=%s",
			got.FieldsResolved, got.FieldsJSON)
	}
}

func TestValidateRoute_FallsBackToTheFirstAnswerWhenNobodyResolves(t *testing.T) {
	unknowns := `{"gate":{"unknown":"stream triggers have no capture profile"}}`
	holder := &pipelineSourceSet[domain.PipelineAuthor]{}
	holder.add(&fakeAuthor{fields: unknowns, resolved: false})
	holder.add(&fakeAuthor{fields: unknowns, resolved: false})

	got, err := deferredPipelineAuthor{holder: holder}.ValidatePipeline(context.Background(), "{}")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// An unresolved projection is still an answer — the named unknowns are
	// honest — it just never outranks a resolved one.
	if got.FieldsJSON != unknowns || got.NodeCount != 3 {
		t.Fatalf("the first answer must stand when none resolves, got %+v", got)
	}
}
