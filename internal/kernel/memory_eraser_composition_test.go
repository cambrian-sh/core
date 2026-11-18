package kernel

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
)

type fakeEraser struct{}

func (fakeEraser) Erase(context.Context, domain.ErasureRequest) (domain.ErasureReceipt, error) {
	return domain.ErasureReceipt{}, nil
}
func (fakeEraser) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}
func (fakeEraser) ListReceipts(context.Context, domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	return nil, nil
}
func (fakeEraser) Resolve(context.Context, domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	return nil, nil
}

// ADR-0137 S3 (ENG-411 stage B): the stack exposes the erasure chokepoint
// (*authz.EnforcingEraser) and nothing else that can erase — no exported field holds an
// eraser, and the only method returning one returns the enforcing decorator. Off
// (execution.erasure.enabled=false, the default) there is no eraser at all. The backend
// field stays optional.
func TestNewMemoryStack_ExposesOnlyTheEnforcingEraser(t *testing.T) {
	cfg := config.DefaultConfig()
	b := MemoryBackend{Vector: plainVec{}, Eraser: fakeEraser{}}
	for _, m := range b.Missing() {
		if m == "Eraser" {
			t.Fatal("Eraser is optional and must not be reported missing")
		}
	}
	if cfg.Execution.Erasure.Enabled {
		t.Fatal("execution.erasure.enabled must default to false (ADR-0137 D10)")
	}
	if NewMemoryStack(b, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{}).Eraser() != nil {
		t.Fatal("erasure off must mean no eraser")
	}

	cfg.Execution.Erasure.Enabled = true
	mem := NewMemoryStack(b, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	if _, ok := mem.Eraser().(*authz.EnforcingEraser); !ok {
		t.Fatalf("Eraser() = %T, want the enforcing decorator", mem.Eraser())
	}
	eraserType := reflect.TypeOf((*domain.MemoryEraser)(nil)).Elem()
	st := reflect.TypeOf(*mem)
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.IsExported() && (f.Type == eraserType || f.Type.Implements(eraserType)) {
			t.Errorf("exported field %s exposes an eraser", f.Name)
		}
	}
	pt := reflect.TypeOf(mem)
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i)
		for o := 0; o < m.Type.NumOut(); o++ {
			out := m.Type.Out(o)
			if (out == eraserType || (out.Kind() != reflect.Interface && out.Implements(eraserType))) && m.Name != "Eraser" {
				t.Errorf("method %s returns an eraser", m.Name)
			}
		}
	}
	if NewMemoryStack(MemoryBackend{Vector: plainVec{}}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{}).Eraser() != nil {
		t.Fatal("no backend eraser must mean no eraser")
	}
}
