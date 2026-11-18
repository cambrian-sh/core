// Package operatortest is test support for code outside the operator package
// that needs a real operator.Service (ENG-1305). operator.New refuses a service
// without its required seams, so a test that exercises one surface (the command
// audit, the forget RPCs, run cancellation) starts from Deps here — every required
// seam an inert stand-in — and replaces the seams it actually tests. Imported by
// tests only (like app/migratetest); no production code may import it.
package operatortest

import (
	"context"
	"errors"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Deps returns a complete operator.Deps over feed (a fresh spool when nil). Each
// stand-in answers emptily or refuses: no session, no tool, no memory, no
// pipeline, and an authenticator that accepts no token.
func Deps(feed *operator.Spool) operator.Deps {
	if feed == nil {
		feed = operator.NewSpool(operator.SpoolConfig{})
	}
	return operator.Deps{
		Feed:              feed,
		Projection:        operator.NewProjection(),
		Sessions:          noSessions{},
		Identity:          noIdentity{},
		Audit:             operator.NewInMemoryAuditStore(),
		Grants:            domain.NewInMemoryGrantsStore(),
		Controls:          operator.NewExecutionControlHub(),
		Approvals:         domain.NewAddressedHub(domain.AddressedHubConfig{}),
		Effects:           operator.CommandEffectsFuncs{},
		Tools:             noTools{},
		Skills:            noSkills{},
		Memory:            noMemory{},
		ToolRunner:        noToolRunner{},
		Ingestor:          operator.MemoryIngestorFunc(noIngest),
		SessionOps:        operator.SessionOpsFuncs{},
		PipelineLister:    noPipelines{},
		PipelineDryRunner: noPipelines{},
		PipelineAuthor:    noPipelines{},
		PipelineWriter:    noPipelines{},
		PipelineLifecycle: noPipelines{},
		KernelVersion:     "test",
		ContractVersion:   "test",
	}
}

// New builds a service from Deps(feed) with mutate applied (nil = none), failing
// the test on a construction error.
func New(t interface {
	Helper()
	Fatalf(string, ...any)
}, feed *operator.Spool, mutate func(*operator.Deps, *operator.Options)) *operator.Service {
	t.Helper()
	d := Deps(feed)
	var o operator.Options
	if mutate != nil {
		mutate(&d, &o)
	}
	svc, err := operator.New(d, o)
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	return svc
}

var errStub = errors.New("operatortest: not wired in this test")

type noSessions struct{}

func (noSessions) ListSessions(context.Context, domain.SessionStatus) ([]domain.Session, error) {
	return nil, nil
}

type noIdentity struct{}

func (noIdentity) Resolve(context.Context, string) (domain.Identity, error) {
	return domain.Identity{}, domain.ErrUnauthenticated
}
func (noIdentity) Revoke(context.Context, string) error { return nil }

type noTools struct{}

func (noTools) AllTools() []domain.SystemTool { return nil }

type noSkills struct{}

func (noSkills) All() []domain.Skill { return nil }

type noMemory struct{}

func (noMemory) SearchSystem(context.Context, string) ([]domain.SearchResult, error) {
	return nil, nil
}

type noToolRunner struct{}

func (noToolRunner) Execute(context.Context, domain.ToolCallRequest) domain.ToolCallResponse {
	return domain.ToolCallResponse{Error: errStub.Error()}
}

func noIngest(context.Context, operator.IngestRequest) (string, error) { return "", errStub }

type noPipelines struct{}

func (noPipelines) ListPipelines(context.Context, bool, string) ([]domain.PipelineSummary, error) {
	return nil, nil
}
func (noPipelines) ListNodeItems(context.Context, string, string, int) ([]domain.NodeItem, error) {
	return nil, nil
}
func (noPipelines) DryRunPipeline(context.Context, string, int, int) (domain.PipelineDryRun, error) {
	return domain.PipelineDryRun{}, domain.ErrPipelineNotFound
}
func (noPipelines) GetPipeline(context.Context, string, int) (domain.PipelineGraph, error) {
	return domain.PipelineGraph{}, domain.ErrPipelineNotFound
}
func (noPipelines) ValidatePipeline(context.Context, string) (domain.PipelineValidation, error) {
	return domain.PipelineValidation{}, errStub
}
func (noPipelines) SavePipeline(context.Context, string) (domain.PipelineSaved, error) {
	return domain.PipelineSaved{}, errStub
}
func (noPipelines) TransitionPipeline(context.Context, string, int, string) (domain.PipelineTransitioned, error) {
	return domain.PipelineTransitioned{}, domain.ErrPipelineNotFound
}
