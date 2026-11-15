package kernel

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

type fakeEraser struct{}

func (fakeEraser) Erase(context.Context, domain.ErasureRequest) (domain.ErasureReceipt, error) {
	return domain.ErasureReceipt{}, nil
}
func (fakeEraser) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}

// ADR-0137 S2 (ENG-411 stage A): the raw eraser is composed into the stack, and the stack
// offers NO exported way to reach it — it enforces nothing about who may erase what, so
// only the S3 enforcing decorator may be exposed (AGENTS.md §2.3). An optional backend
// field: its absence is not a missing capability.
func TestNewMemoryStack_HoldsTheRawEraserUnexported(t *testing.T) {
	cfg := config.DefaultConfig()
	b := MemoryBackend{Vector: plainVec{}, Eraser: fakeEraser{}}
	for _, m := range b.Missing() {
		if m == "Eraser" {
			t.Fatal("Eraser is optional and must not be reported missing")
		}
	}
	mem := NewMemoryStack(b, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{})
	if mem.eraser == nil {
		t.Fatal("the backend's eraser was not composed into the stack")
	}
	eraserType := reflect.TypeOf((*domain.MemoryEraser)(nil)).Elem()
	st := reflect.TypeOf(*mem)
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.IsExported() && (f.Type == eraserType || f.Type.Implements(eraserType)) {
			t.Errorf("exported field %s exposes the raw eraser", f.Name)
		}
	}
	pt := reflect.TypeOf(mem)
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i)
		for o := 0; o < m.Type.NumOut(); o++ {
			if out := m.Type.Out(o); out == eraserType || (out.Kind() != reflect.Interface && out.Implements(eraserType)) {
				t.Errorf("method %s returns the raw eraser", m.Name)
			}
		}
	}
	if NewMemoryStack(MemoryBackend{Vector: plainVec{}}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}).eraser != nil {
		t.Fatal("no backend eraser must mean no eraser")
	}
}
