package operator

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type depsSessions struct{}

func (depsSessions) ListSessions(context.Context, domain.SessionStatus) ([]domain.Session, error) {
	return nil, nil
}

type depsIdentity struct{}

func (depsIdentity) Resolve(context.Context, string) (domain.Identity, error) {
	return domain.Identity{}, domain.ErrUnauthenticated
}
func (depsIdentity) Revoke(context.Context, string) error { return nil }

type depsTools struct{}

func (depsTools) AllTools() []domain.SystemTool { return nil }

type depsSkills struct{}

func (depsSkills) All() []domain.Skill { return nil }

type depsMemory struct{}

func (depsMemory) SearchSystem(context.Context, string) ([]domain.SearchResult, error) {
	return nil, nil
}

type depsRunner struct{}

func (depsRunner) Execute(context.Context, domain.ToolCallRequest) domain.ToolCallResponse {
	return domain.ToolCallResponse{}
}

type depsPipelines struct{}

func (depsPipelines) ListPipelines(context.Context, bool, string) ([]domain.PipelineSummary, error) {
	return nil, nil
}
func (depsPipelines) ListNodeItems(context.Context, string, string, int) ([]domain.NodeItem, error) {
	return nil, nil
}
func (depsPipelines) DryRunPipeline(context.Context, string, int, int) (domain.PipelineDryRun, error) {
	return domain.PipelineDryRun{}, nil
}
func (depsPipelines) GetPipeline(context.Context, string, int) (domain.PipelineGraph, error) {
	return domain.PipelineGraph{}, nil
}
func (depsPipelines) ValidatePipeline(context.Context, string) (domain.PipelineValidation, error) {
	return domain.PipelineValidation{}, nil
}
func (depsPipelines) SavePipeline(context.Context, string) (domain.PipelineSaved, error) {
	return domain.PipelineSaved{}, nil
}
func (depsPipelines) TransitionPipeline(context.Context, string, int, string) (domain.PipelineTransitioned, error) {
	return domain.PipelineTransitioned{}, nil
}

func fullDeps() Deps {
	return Deps{
		Feed: NewSpool(SpoolConfig{}), Projection: NewProjection(), Sessions: depsSessions{},
		Identity: depsIdentity{}, Audit: NewInMemoryAuditStore(), Grants: domain.NewInMemoryGrantsStore(),
		Controls: NewExecutionControlHub(), Approvals: domain.NewAddressedHub(domain.AddressedHubConfig{}),
		Effects: CommandEffectsFuncs{}, Tools: depsTools{}, Skills: depsSkills{}, Memory: depsMemory{},
		ToolRunner: depsRunner{},
		Ingestor:   MemoryIngestorFunc(func(context.Context, IngestRequest) (string, error) { return "", nil }),
		SessionOps: SessionOpsFuncs{}, PipelineLister: depsPipelines{}, PipelineDryRunner: depsPipelines{},
		PipelineAuthor: depsPipelines{}, PipelineWriter: depsPipelines{}, PipelineLifecycle: depsPipelines{},
		KernelVersion: "v-test", ContractVersion: "0108", Capabilities: []string{"feed"},
	}
}

// ENG-1305: a composition that forgets a required seam is refused at construction,
// with every missing seam named, instead of answering Unimplemented at call time.
func TestNew_RefusesEveryMissingRequiredSeam(t *testing.T) {
	_, err := New(Deps{}, Options{})
	if !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("New(Deps{}) = %v, want ErrMissingSeam", err)
	}
	want := []string{"Feed", "Projection", "Sessions", "Identity", "Audit", "Grants", "Controls",
		"Approvals", "Effects", "Tools", "Skills", "Memory", "ToolRunner", "Ingestor", "SessionOps",
		"PipelineLister", "PipelineDryRunner", "PipelineAuthor", "PipelineWriter", "PipelineLifecycle",
		"KernelVersion", "ContractVersion"}
	for _, name := range want {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not name %s", err, name)
		}
	}
	// One at a time: each required seam alone is refused.
	for _, name := range want {
		d := fullDeps()
		reflect.ValueOf(&d).Elem().FieldByName(name).Set(reflect.Zero(reflect.ValueOf(d).FieldByName(name).Type()))
		if _, err := New(d, Options{}); !errors.Is(err, ErrMissingSeam) || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: New = %v, want ErrMissingSeam naming it", name, err)
		}
	}
}

// A typed nil (an interface holding a nil pointer) is as absent as a nil interface:
// a plain `== nil` would accept it and the RPC would nil-dereference at call time.
func TestNew_TypedNilIsMissing(t *testing.T) {
	d := fullDeps()
	var audit *InMemoryAuditStore
	d.Audit = audit
	var hub *domain.AddressedHub
	d.Approvals = hub
	_, err := New(d, Options{})
	if !errors.Is(err, ErrMissingSeam) || !strings.Contains(err.Error(), "Audit") || !strings.Contains(err.Error(), "Approvals") {
		t.Fatalf("typed-nil Audit/Approvals: New = %v, want ErrMissingSeam naming both", err)
	}
}

// Optional seams left nil are reported (sorted) for the one boot log line; a set
// one is not, and configuration values (SourceMetaCaps) are never listed.
func TestNew_UnwiredNamesTheNilOptionalSeams(t *testing.T) {
	svc, err := New(fullDeps(), Options{LogRing: nil, TokenSeries: nil})
	if err != nil {
		t.Fatal(err)
	}
	u := svc.Unwired()
	if !sort.StringsAreSorted(u) {
		t.Fatalf("Unwired not sorted: %v", u)
	}
	n := reflect.TypeOf(Options{}).NumField()
	// Every seam field (all but SourceMetaCaps and the Plugins slice) is unwired here.
	if len(u) != n-2 {
		t.Fatalf("Unwired = %d names %v, want %d", len(u), u, n-2)
	}
	for _, not := range []string{"SourceMetaCaps", "Plugins"} {
		for _, name := range u {
			if name == not {
				t.Fatalf("Unwired lists the value field %s", not)
			}
		}
	}
	svc, err = New(fullDeps(), Options{TokenSeries: fakeTokenSeries{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range svc.Unwired() {
		if name == "TokenSeries" {
			t.Fatal("a wired seam is reported unwired")
		}
	}
	if !svc.HasTokenSeries() {
		t.Fatal("Options.TokenSeries did not reach the service")
	}
}

type fakeTokenSeries struct{ domain.TokenSeriesReader }

// The required seams reach the service and the handshake is the one given.
func TestNew_WiresTheRequiredSeams(t *testing.T) {
	d := fullDeps()
	svc, err := New(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.feed != d.Feed || svc.projection != d.Projection || svc.idp != d.Identity || svc.audit != d.Audit ||
		svc.controls != d.Controls || svc.hitl != d.Approvals || svc.kernelVersion != "v-test" ||
		svc.contractVersion != "0108" || !reflect.DeepEqual(svc.capabilities, []string{"feed"}) {
		t.Fatal("a required seam or the handshake did not reach the service")
	}
	if svc.logins == nil {
		t.Fatal("the login limiter must be built by New")
	}
}

// Static guard: New must assign every seam field of Service, so a seam added to
// the struct without a constructor argument fails here rather than being
// attachable only by a test-only setter.
func TestNew_AssignsEveryServiceSeamField(t *testing.T) {
	fset := token.NewFileSet()
	assigned := map[string]bool{}
	f, err := parser.ParseFile(fset, "service_deps.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "New" && fn.Name.Name != "newService") {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
							assigned[sel.Sel.Name] = true
						}
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok {
					assigned[id.Name] = true
				}
			}
			return true
		})
		return false
	})
	internal := map[string]bool{"UnimplementedOperatorConsoleServer": true, "grantsMu": true}
	st := reflect.TypeOf(Service{})
	var missing []string
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if !internal[name] && !assigned[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Service fields New never assigns (add them to Deps or Options): %v", missing)
	}
}
