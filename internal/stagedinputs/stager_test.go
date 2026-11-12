package stagedinputs_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/stagedinputs"
)

// The executor + stager together, with the real enforced ledger reader: what a
// staged call can see is exactly what its caller could read, no more, and nothing
// outlives the call.

const (
	sandboxTool = "mcp:code-sandbox/execute_code"
	readerTool  = stagedinputs.DefaultTextReader
	mediaTool   = stagedinputs.DefaultBinaryReader
)

// forbidSecret is a decision point under which agent callers may not read rows
// tagged "secret".
type forbidSecret struct{ domain.AllowAllAuthorizer }

func (forbidSecret) ReadFilter(_ context.Context, p domain.PrincipalRef, _ domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return &domain.TagPredicate{ForbiddenTags: []string{"secret"}}, domain.AccessDecision{Allowed: true, Principal: p}
}

// memLedger is the raw store (the enforced reader wraps it) plus a writer that
// remembers what it was asked to record.
type memLedger struct {
	mu      sync.Mutex
	rows    map[string]domain.LedgerEntry
	records []domain.LedgerRecord
}

func (m *memLedger) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (m *memLedger) Find(context.Context, domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	return nil, nil
}

func (m *memLedger) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	id := fmt.Sprintf("led_rec%d", len(m.records))
	return domain.LedgerEntry{ID: id, Kind: rec.Kind}, nil
}

func content(text string) []byte {
	b, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	return b
}

// sandboxHandler plays the MCP side: the reader tools return file contents, and
// execute_code looks at what it was given (as the server would mount it) and
// writes an output file.
type sandboxHandler struct {
	mu        sync.Mutex
	meta      map[string]any
	seenFiles map[string]string // name -> content, as found in the inputs folder during the call
	sawRunDir bool
	calls     []string
}

func (h *sandboxHandler) Execute(_ context.Context, call domain.ToolCall) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call.ToolName)
	switch call.ToolName {
	case readerTool:
		return content("date,amount\n2026-11-01,12.50\n"), nil
	case mediaTool:
		b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "blob", "mimeType": "application/octet-stream",
			"data": base64.StdEncoding.EncodeToString([]byte("PK\x03\x04xlsx-bytes"))}}})
		return b, nil
	case sandboxTool:
		h.meta = call.Meta
		if dir, ok := call.Meta[domain.StagedInputsMetaDir].(string); ok {
			h.sawRunDir = true
			h.seenFiles = map[string]string{}
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				h.seenFiles[e.Name()] = string(b)
			}
			out, _ := call.Meta[domain.StagedInputsMetaOutputs].(string)
			_ = os.WriteFile(filepath.Join(out, "summary.csv"), []byte("total\n12.50\n"), 0o600)
		}
		return content("ok"), nil
	}
	return nil, fmt.Errorf("unexpected tool %s", call.ToolName)
}

type fixture struct {
	exec    *domain.ToolExecutor
	handler *sandboxHandler
	ledger  *memLedger
	root    string
}

func newFixture(t *testing.T, withLedger bool) *fixture {
	t.Helper()
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: sandboxTool, StagedInputs: true, Effects: []domain.ToolEffect{domain.EffectRead}})
	reg.Register(domain.SystemTool{Name: readerTool, Effects: []domain.ToolEffect{domain.EffectRead}})
	reg.Register(domain.SystemTool{Name: mediaTool, Effects: []domain.ToolEffect{domain.EffectRead}})
	led := &memLedger{rows: map[string]domain.LedgerEntry{
		"led_bank": {ID: "led_bank", Kind: domain.LedgerToolResult, Tags: []string{"finance"},
			Args: json.RawMessage(`{"path":"/home/person/Finance/bank-export.csv"}`), Body: content("date,amount\n2026-11-02,40.00\n")},
		"led_secret": {ID: "led_secret", Kind: domain.LedgerToolResult, Tags: []string{"secret"},
			Args: json.RawMessage(`{"path":"/hr/salaries.csv"}`), Body: content("name,salary\n")},
		"led_derived_secret": {ID: "led_derived_secret", Kind: domain.LedgerStepOutput, Tags: []string{"finance"},
			SourceTags: [][]string{{"secret"}}, Body: []byte(`{"answer":"x"}`)},
		"led_cut": {ID: "led_cut", Kind: domain.LedgerToolResult, Tags: []string{"finance"}, Truncated: true, SizeBytes: 999, Body: []byte("[{")},
	}}
	root := t.TempDir()
	var reader domain.LedgerReader
	if withLedger {
		reader = authz.NewEnforcingLedger(led, nil)
	}
	h := &sandboxHandler{}
	ex := &domain.ToolExecutor{Registry: reg, Handler: h, MCPHandler: h, Unrestricted: true,
		Authz: forbidSecret{}, InlineThreshold: 1 << 20,
		Stager: stagedinputs.New(stagedinputs.Config{Root: root}, reader)}
	if withLedger {
		ex.Ledger = led
	}
	return &fixture{exec: ex, handler: h, ledger: led, root: root}
}

func (f *fixture) run(t *testing.T, inputs ...string) domain.ToolCallResponse {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"code": "print(1)", "inputs": inputs})
	return f.exec.Execute(context.Background(), domain.ToolCallRequest{AgentID: "worker", ToolName: sandboxTool, ArgsJSON: args})
}

func runDirs(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestStagedCall_FolderHoldsExactlyTheDeclaredInputs_AndIsRemoved(t *testing.T) {
	f := newFixture(t, true)
	resp := f.run(t, "led_bank", "Finance/notes.txt")
	if resp.Denied || resp.Error != "" {
		t.Fatalf("call refused: %+v", resp)
	}
	names := make([]string, 0, len(f.handler.seenFiles))
	for n := range f.handler.seenFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "bank-export.csv,notes.txt" {
		t.Fatalf("inputs folder held %v, want exactly the two declared inputs", names)
	}
	if got := f.handler.seenFiles["bank-export.csv"]; got != "date,amount\n2026-11-02,40.00\n" {
		t.Fatalf("ledger input content = %q", got)
	}
	files, _ := f.handler.meta[domain.StagedInputsMetaFiles].(map[string]any)
	if files["led_bank"] != "bank-export.csv" || files["Finance/notes.txt"] != "notes.txt" {
		t.Fatalf("meta input map = %v", files)
	}
	// The path input was read through the reader tool as the caller.
	if strings.Join(f.handler.calls, ",") != readerTool+","+sandboxTool {
		t.Fatalf("calls = %v", f.handler.calls)
	}
	if left := runDirs(t, f.root); len(left) != 0 {
		t.Fatalf("per-run folder not removed after the call: %v", left)
	}
}

func TestStagedCall_ForbiddenLedgerIDIsRefusedAndNeverStaged(t *testing.T) {
	for _, id := range []string{"led_secret", "led_derived_secret", "led_missing"} {
		f := newFixture(t, true)
		resp := f.run(t, "led_bank", id)
		if !resp.Denied || !strings.Contains(resp.DenyReason, id) {
			t.Fatalf("%s: want a refusal naming it, got %+v", id, resp)
		}
		if f.handler.sawRunDir || len(f.handler.calls) != 0 {
			t.Fatalf("%s: the tool ran (or a read happened) for a refused staging: %v", id, f.handler.calls)
		}
		if left := runDirs(t, f.root); len(left) != 0 {
			t.Fatalf("%s: a folder was left behind: %v", id, left)
		}
		if strings.Contains(resp.DenyReason, "salary") {
			t.Fatalf("refusal leaked content: %s", resp.DenyReason)
		}
	}
}

func TestStagedCall_TruncatedEntryRefused(t *testing.T) {
	f := newFixture(t, true)
	resp := f.run(t, "led_cut")
	if !resp.Denied || !strings.Contains(resp.DenyReason, "by its path") {
		t.Fatalf("want a refusal pointing at the path, got %+v", resp)
	}
}

func TestStagedCall_OutputsLandInTheLedgerDerivedFromInputs(t *testing.T) {
	f := newFixture(t, true)
	resp := f.run(t, "led_bank")
	if resp.Denied || resp.Error != "" {
		t.Fatalf("call refused: %+v", resp)
	}
	var artifact, result *domain.LedgerRecord
	for i := range f.ledger.records {
		r := &f.ledger.records[i]
		switch {
		case r.Kind == domain.LedgerArtifact:
			artifact = r
		case r.Kind == domain.LedgerToolResult && r.ToolName == sandboxTool:
			result = r
		}
	}
	if artifact == nil || string(artifact.Body) != "total\n12.50\n" || !strings.Contains(string(artifact.ArgsJSON), "outputs/summary.csv") {
		t.Fatalf("output file not recorded as an artifact: %+v", f.ledger.records)
	}
	if strings.Join(artifact.DerivedFrom, ",") != "led_bank" || artifact.Writer != domain.AgentPrincipal("worker") {
		t.Fatalf("artifact provenance: derived=%v writer=%v", artifact.DerivedFrom, artifact.Writer)
	}
	if result == nil || strings.Join(result.DerivedFrom, ",") != "led_bank" {
		t.Fatalf("the call's own result is not derived from its inputs: %+v", result)
	}
	if !strings.Contains(string(resp.ResultJSON), "summary.csv") || !strings.Contains(string(resp.ResultJSON), "led_rec") {
		t.Fatalf("result does not name the recorded output: %s", resp.ResultJSON)
	}
}

func TestStagedCall_BinaryPathUsesTheMediaReader(t *testing.T) {
	f := newFixture(t, true)
	resp := f.run(t, "/home/person/Finance/budget.xlsx")
	if resp.Denied || resp.Error != "" {
		t.Fatalf("call refused: %+v", resp)
	}
	if got := f.handler.seenFiles["budget.xlsx"]; got != "PK\x03\x04xlsx-bytes" {
		t.Fatalf("binary input = %q", got)
	}
}

func TestStagedCall_NoInputsRunsUnstaged_AndNoStagerRefusesInputs(t *testing.T) {
	f := newFixture(t, true)
	resp := f.run(t)
	if resp.Denied || f.handler.sawRunDir || f.handler.meta != nil {
		t.Fatalf("a call without inputs was staged or refused: %+v meta=%v", resp, f.handler.meta)
	}
	f = newFixture(t, true)
	f.exec.Stager = nil
	resp = f.run(t, "led_bank")
	if !resp.Denied || !strings.Contains(resp.DenyReason, "staged_inputs") || len(f.handler.calls) != 0 {
		t.Fatalf("inputs without a stager must be refused before the tool runs: %+v", resp)
	}
}

func TestStagedCall_LedgerOffRefusesLedgerIDsButStagesPaths(t *testing.T) {
	f := newFixture(t, false)
	if resp := f.run(t, "led_bank"); !resp.Denied || !strings.Contains(resp.DenyReason, "ledger is off") {
		t.Fatalf("want a ledger-off refusal, got %+v", resp)
	}
	f = newFixture(t, false)
	resp := f.run(t, "Finance/notes.txt")
	if resp.Denied || f.handler.seenFiles["notes.txt"] == "" {
		t.Fatalf("path input not staged with the ledger off: %+v", resp)
	}
	if !strings.Contains(string(resp.ResultJSON), "not recorded") {
		t.Fatalf("result should say the output was not recorded: %s", resp.ResultJSON)
	}
}

func TestStagedCall_RefusedPathReadRefusesTheCall(t *testing.T) {
	f := newFixture(t, true)
	f.exec.Registry.(*domain.InMemoryToolRegistry).Remove(readerTool) // the caller has no way to read paths
	resp := f.run(t, "Finance/notes.txt")
	if !resp.Denied || !strings.Contains(resp.DenyReason, "Finance/notes.txt") || f.handler.sawRunDir {
		t.Fatalf("want a refusal, got %+v", resp)
	}
}

func TestStagedCall_TooManyInputs(t *testing.T) {
	f := newFixture(t, true)
	many := make([]string, stagedinputs.DefaultMaxFiles+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d.txt", i)
	}
	if resp := f.run(t, many...); !resp.Denied || !strings.Contains(resp.DenyReason, "at most") {
		t.Fatalf("want a cap refusal, got %+v", resp)
	}
}

func TestStagedInputsOf(t *testing.T) {
	cases := map[string]struct {
		args string
		want string
		ok   bool
	}{
		"list":    {`{"inputs":["led_a"," b.csv ",""]}`, "led_a,b.csv", true},
		"string":  {`{"inputs":"led_a"}`, "led_a", true},
		"absent":  {`{"code":"x"}`, "", true},
		"null":    {`{"inputs":null}`, "", true},
		"invalid": {`{"inputs":{"a":1}}`, "", false},
	}
	for name, c := range cases {
		got, ok := domain.StagedInputsOf([]byte(c.args), "inputs")
		if ok != c.ok || strings.Join(got, ",") != c.want {
			t.Errorf("%s: got %v %v", name, got, ok)
		}
	}
}
