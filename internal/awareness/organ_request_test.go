package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// Shared helpers for the awareness organs' C1 request tests (ADR-0139 D1/D2):
// a fake RequestGenerator that records the structured request, a golden
// renderer of exactly what is sent, and a schema-vs-decoder check.

var updatePromptGolden = flag.Bool("update-prompt-golden", false,
	"rewrite testdata/*.golden from the rendered organ requests")

// requestRecorder is a C1 generator: it records every GenerateRequest and
// answers with reply. Its legacy Generate fails, so a test using it proves the
// organ took the structured path.
type requestRecorder struct {
	reqs    []domain.GenerateRequest
	reply   string
	ignored []string
	err     error
}

func (r *requestRecorder) Generate(context.Context, string) (string, error) {
	return "", errors.New("requestRecorder: the legacy string path was used")
}

func (r *requestRecorder) GenerateRequest(_ context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return domain.GenerateResponse{}, r.err
	}
	return domain.GenerateResponse{Text: r.reply, StopReason: domain.StopEndTurn, Ignored: r.ignored}, nil
}

func (r *requestRecorder) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return nil, errors.New("requestRecorder: no streaming")
}

func (r *requestRecorder) Capabilities() domain.Capabilities { return domain.Capabilities{} }

func (r *requestRecorder) only(t *testing.T) domain.GenerateRequest {
	t.Helper()
	if len(r.reqs) != 1 {
		t.Fatalf("%d structured requests, want 1", len(r.reqs))
	}
	return r.reqs[0]
}

// renderRequest is the golden form of a request: every field the model is sent.
func renderRequest(req domain.GenerateRequest) string {
	var b strings.Builder
	for i, s := range req.System {
		fmt.Fprintf(&b, "=== system[%d] cache=%v ===\n%s\n", i, s.Cache, s.Text)
	}
	for i, m := range req.Messages {
		fmt.Fprintf(&b, "=== messages[%d] %s ===\n%s\n", i, m.Role, m.Content)
	}
	if rs := req.ResponseSchema; rs != nil {
		fmt.Fprintf(&b, "=== response_schema %s strict=%v ===\n%s\n", rs.Name, rs.Strict, strings.TrimSpace(string(rs.Schema)))
	}
	fmt.Fprintf(&b, "=== cache_ttl %q ===\n", req.CacheTTL)
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updatePromptGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-prompt-golden): %v", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatalf("%s is stale or the prompt changed; regenerate with -update-prompt-golden\n--- got ---\n%s", name, got)
	}
}

// assertPairRequest checks the C1 layout every awareness organ sends: one cached
// system block that is exactly the pair's stable prompt, one user message that is
// the pair's input template rendered (task last), the pair's schema, and ttl.
func assertPairRequest(t *testing.T, req domain.GenerateRequest, pair prompts.Pair, ttl domain.CacheTTL, lastLine string) {
	t.Helper()
	if len(req.System) != 1 || !req.System[0].Cache || req.System[0].Text != pair.System.Body {
		t.Fatalf("system must be the cached %s prompt, got %+v", pair.System.ID, req.System)
	}
	if len(pair.System.Inputs) != 0 {
		t.Fatalf("%s must have no slots: its bytes are the cached prefix", pair.System.ID)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != domain.RoleUser {
		t.Fatalf("want one user message, got %+v", req.Messages)
	}
	if !strings.HasSuffix(req.Messages[0].Content, lastLine) {
		t.Fatalf("the task must come last (%q):\n%s", lastLine, req.Messages[0].Content)
	}
	if req.ResponseSchema == nil || string(req.ResponseSchema.Schema) != string(pair.Schema()) || len(pair.Schema()) == 0 {
		t.Fatalf("response schema must be %s's schema file", pair.System.ID)
	}
	if req.CacheTTL != ttl {
		t.Fatalf("cache_ttl = %q, want %q", req.CacheTTL, ttl)
	}
}

// ── Schema vs decoder ────────────────────────────────────────────────────────

// checkSchemaMatchesType walks a response schema and the Go type its reply is
// decoded into: every schema property must be a JSON field of the type (or the
// model is asked for data the decoder silently drops), every required name must
// be a property, and every object must forbid additional properties (constrained
// decoding providers require it, and an extra field is data the decoder drops).
func checkSchemaMatchesType(t *testing.T, where string, schema map[string]any, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	switch schema["type"] {
	case "object":
		if schema["additionalProperties"] != false {
			t.Errorf("%s: object must set additionalProperties: false", where)
		}
		props, _ := schema["properties"].(map[string]any)
		fields := jsonFields(typ)
		for name, sub := range props {
			f, ok := fields[name]
			if !ok {
				t.Errorf("%s: schema property %q has no field in %s", where, name, typ)
				continue
			}
			checkSchemaMatchesType(t, where+"."+name, sub.(map[string]any), f)
		}
		req, _ := schema["required"].([]any)
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s: required %q is not a property", where, r)
			}
		}
	case "array":
		checkSchemaMatchesType(t, where+"[]", schema["items"].(map[string]any), typ)
	}
	for _, banned := range []string{"minimum", "maximum", "minLength", "maxLength", "$schema"} {
		if _, ok := schema[banned]; ok {
			t.Errorf("%s: %q is not accepted by every constrained-decoding provider", where, banned)
		}
	}
}

// jsonFields maps JSON names to field types, including promoted embedded fields.
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	if typ.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Anonymous && name == "" {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func schemaOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sortedKeys(m map[string]reflect.Type) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestOrganSchemasMatchTheirDecoders(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pair prompts.Pair
		typ  reflect.Type
	}{
		{plannerPrompts, reflect.TypeOf(planReply{})},
		{plannerCapPrompts, reflect.TypeOf(planReply{})},
		{replanPrompts, reflect.TypeOf(planReply{})},
		{replanCapPrompts, reflect.TypeOf(planReply{})},
		{clarifierPrompts, reflect.TypeOf(modelClarification{})},
	} {
		checkSchemaMatchesType(t, c.pair.System.ID, schemaOf(t, c.pair.Schema()), c.typ)
	}
	if got := sortedKeys(jsonFields(reflect.TypeOf(planReply{}))); !strings.Contains(strings.Join(got, ","), "reasoning") {
		t.Fatalf("planReply must decode reasoning, has %v", got)
	}
}

// The planner and the replanner must ask for the same plan shape on each arm, or
// a replanned plan could carry fields a planned one cannot (and vice versa).
func TestReplanSchemaIsThePlannerSchema(t *testing.T) {
	t.Parallel()
	if string(replanPrompts.Schema()) != string(plannerPrompts.Schema()) {
		t.Error("prompts/replan/plan.schema.json drifted from prompts/planner/plan.schema.json")
	}
	if string(replanCapPrompts.Schema()) != string(plannerCapPrompts.Schema()) {
		t.Error("prompts/replan_cap/plan_cap.schema.json drifted from prompts/planner_cap/plan_cap.schema.json")
	}
}

// ── decodeReply ──────────────────────────────────────────────────────────────

func TestDecodeReply_ValidatedIsStrictUncheckedToleratesOnlyAFence(t *testing.T) {
	t.Parallel()
	const js = `{"answerable":true,"reason":"","questions":[]}`
	unchecked := []string{domain.FieldResponseSchema}
	for _, c := range []struct {
		name    string
		text    string
		ignored []string
		ok      bool
	}{
		{"validated", js, nil, true},
		{"validated fenced is not JSON", "```json\n" + js + "\n```", nil, false},
		{"unchecked fenced", "```json\n" + js + "\n```", unchecked, true},
		{"unchecked plain", js, unchecked, true},
		{"unchecked prose", "Here you go: " + js, unchecked, false},
		{"unchecked thought preamble", "<thought>hm</thought>" + js, unchecked, false},
	} {
		var m modelClarification
		err := decodeReply(domain.GenerateResponse{Text: c.text, Ignored: c.ignored}, &m)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	if got := truncateRunes("héllo wörld", 4); got != "héll" {
		t.Fatalf("got %q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
