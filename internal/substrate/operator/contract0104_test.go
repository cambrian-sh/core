package operator

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// Contract 0104, ENG-461: the fact lane's per-hit annotations reach MemoryOp.
func TestSetFactAnnotations(t *testing.T) {
	op := &pb.MemoryOp{}
	setFactAnnotations(map[string]interface{}{
		domain.MetaFactMatches:    []string{"user home_city = Paris (current)"},
		domain.MetaFactMatchScore: 0.83,
		domain.MetaFactCurrent:    true,
	}, op)
	if len(op.GetFactMatches()) != 1 || op.GetFactMatch() != 0.83 || !op.GetFactCurrent() {
		t.Fatalf("op = %+v", op)
	}
	// A hit the lane did not touch stays unset; a JSON round-trip ([]interface{}) works too.
	plain := &pb.MemoryOp{}
	setFactAnnotations(map[string]interface{}{"tags": []string{"x"}}, plain)
	if plain.GetFactMatches() != nil || plain.GetFactMatch() != 0 || plain.GetFactCurrent() {
		t.Fatalf("untouched hit = %+v", plain)
	}
	rt := &pb.MemoryOp{}
	setFactAnnotations(map[string]interface{}{domain.MetaFactMatches: []interface{}{"a", "b"}}, rt)
	if len(rt.GetFactMatches()) != 2 {
		t.Fatalf("[]interface{} = %+v", rt)
	}
}

type fakeSchema struct{}

func (fakeSchema) SchemaJSON() (string, string, string) { return "{}", "3", "h" }
func (fakeSchema) EditableKeys() []string               { return []string{"execution.x"} }
func (fakeSchema) KernelOnlyKeys() []string             { return nil }
func (fakeSchema) CurrentValues() map[string]float64    { return map[string]float64{"execution.x": 0.5} }
func (fakeSchema) ValueSource() map[string]string       { return map[string]string{"execution.x": "store"} }
func (fakeSchema) FileOverrides() (map[string]string, map[string]string) {
	return map[string]string{"execution.x": "tuning.json"}, map[string]string{"execution.x": "0.3"}
}

// Contract 0104, ENG-257: GetConfigSchema names the file a stored key overrides.
func TestGetConfigSchema_CarriesFileOverrides(t *testing.T) {
	s := &Service{}
	s.SetConfigSchemaReporter(fakeSchema{})
	op, err := s.GetConfigSchema(context.Background(), &pb.GetConfigSchemaOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetOverridesFile()["execution.x"] != "tuning.json" || op.GetOverridesFileValue()["execution.x"] != "0.3" {
		t.Fatalf("op = %+v", op)
	}
}
