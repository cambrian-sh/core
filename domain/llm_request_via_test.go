package domain

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type viaLegacy struct{ got string }

func (g *viaLegacy) Generate(_ context.Context, p string) (string, error) {
	g.got = p
	return "ok", nil
}

func TestGenerateVia_LegacyFlattensAndReportsIgnored(t *testing.T) {
	g := &viaLegacy{}
	req := GenerateRequest{
		System:         []SystemBlock{{Text: "rules", Cache: true}},
		Messages:       []ModelMessage{UserMessage("<data>x</data>")},
		ResponseSchema: &ResponseSchema{Name: "s", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	resp, err := GenerateVia(context.Background(), g, req)
	if err != nil || resp.Text != "ok" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if !strings.HasPrefix(g.got, "rules\n\n<data>x</data>\n\n") || !strings.Contains(g.got, `{"type":"object"}`) {
		t.Fatalf("flattened = %q", g.got)
	}
	if !reflect.DeepEqual(resp.Ignored, []string{FieldSystem, FieldResponseSchema}) || !resp.SchemaUnchecked() {
		t.Fatalf("ignored = %v", resp.Ignored)
	}
	if (GenerateResponse{}).SchemaUnchecked() {
		t.Fatal("a response with nothing ignored is checked")
	}
}
