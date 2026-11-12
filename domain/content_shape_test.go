package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a, run 20261112T132056Z p06:
// the code sandbox returned its chart as a base64 image block AND saved it as an
// output file, so a 600-char stdout travelled as a ~300k-char result.

type fakeStagedCall struct{ outs []StagedOutput }

func (fakeStagedCall) Meta() map[string]any      { return nil }
func (fakeStagedCall) Sources() []string         { return []string{"led_input"} }
func (f fakeStagedCall) Outputs() []StagedOutput { return f.outs }
func (fakeStagedCall) Cleanup()                  {}

func pngBytes(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x89PNG\r\n\x1a\n")
	for i := 8; i < n; i++ {
		b[i] = byte(i * 7)
	}
	return b
}

func mcpImageResult(img []byte, stdout string) []byte {
	b, _ := json.Marshal([]map[string]string{
		{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(img)},
		{"type": "text", "text": stdout},
	})
	return b
}

func TestRecordStagedOutputs_ImageBlockBecomesRef(t *testing.T) {
	img := pngBytes(241539)
	stdout := `{"exit_code":0,"stdout":"Chart created successfully!"}`
	e, led := ledgerExec(t, &fakeHandler{}, SystemTool{Name: "mcp:code-sandbox/execute_code"})
	req := ToolCallRequest{AgentID: "worker", ToolName: "mcp:code-sandbox/execute_code"}
	tool := SystemTool{Name: req.ToolName}
	sc := fakeStagedCall{outs: []StagedOutput{{Name: "chart.png", ContentType: "image/png", Body: img}}}

	result, recorded := e.recordStagedOutputs(context.Background(), req, tool, sc, mcpImageResult(img, stdout))
	result = e.referenceBinaryBlocks(context.Background(), req, tool, result, recorded, sc.Sources())

	if strings.Contains(string(result), base64.StdEncoding.EncodeToString(img[:64])) {
		t.Fatal("the base64 image is still in the result")
	}
	if len(led.recs) != 1 || led.recs[0].Kind != LedgerArtifact {
		t.Fatalf("recorded %d entries, want ONE artifact (the staged file; the image block must reuse it)", len(led.recs))
	}
	var artifactID string
	for _, id := range recorded {
		artifactID = id
	}
	if !strings.Contains(string(result), "[image/png image, 236 KB -> ledger "+artifactID+"]") {
		t.Fatalf("image block not replaced by a reference to the staged artifact: %s", result)
	}
	if !strings.Contains(string(result), "Chart created successfully!") {
		t.Fatal("a text block was touched")
	}
	if len(result) > 2000 {
		t.Fatalf("result is still %d bytes", len(result))
	}
}

// An image that is not a staged output is recorded once as its own artifact.
func TestExecute_UnstagedImageBlockIsRecordedOnce(t *testing.T) {
	img := pngBytes(5000)
	e, led := ledgerExec(t, &fakeHandler{result: mcpImageResult(img, "ok")}, SystemTool{Name: "shot"})
	e.InlineThreshold = 1 << 20
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "shot"})
	if strings.Contains(string(out.ResultJSON), `"data"`) || !strings.Contains(string(out.ResultJSON), "-> ledger led_") {
		t.Fatalf("result = %s", out.ResultJSON)
	}
	if len(led.recs) != 2 || led.recs[0].Kind != LedgerArtifact || string(led.recs[0].Body) != string(img) || led.recs[1].Kind != LedgerToolResult {
		t.Fatalf("records = %d (want the image artifact, then the shaped tool result)", len(led.recs))
	}
	if strings.Contains(string(led.recs[1].Body), `"data"`) {
		t.Fatal("the ledger holds the base64 again in the tool result")
	}
}

func TestExecute_OffloadedResultCarriesPreviewAndSize(t *testing.T) {
	text := "Quarterly report\n" + strings.Repeat("line of findings\n", 400)
	body, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	e, _ := ledgerExec(t, &fakeHandler{result: body}, SystemTool{Name: "mcp:docs/read"})
	e.InlineThreshold, e.PreviewChars, e.PageBytes = 1024, 100, 2000
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "mcp:docs/read"})
	if out.ResultJSON != nil || !IsLedgerID(out.ResultCID) {
		t.Fatalf("not offloaded: %+v", out)
	}
	s := out.Offloaded
	if s == nil {
		t.Fatal("an offloaded result must carry a summary, never a bare id")
	}
	if s.Ref != out.ResultCID || s.SizeBytes != len(body) || s.ContentType != "application/json" ||
		!strings.HasPrefix(s.Shape, "MCP content array: 1 blocks (text)") || s.Pages < 2 {
		t.Fatalf("summary = %+v", s)
	}
	if !strings.HasPrefix(s.Preview, "Quarterly report\nline of") || len(s.Preview) > 100 {
		t.Fatalf("preview must be the readable text, bounded: %q", s.Preview)
	}
	if !strings.Contains(s.Read, "ledger_get id='"+s.Ref+"' page=1..") || !strings.Contains(s.Read, "find=") {
		t.Fatalf("read hint = %q", s.Read)
	}
	var wire map[string]OffloadSummary
	if err := json.Unmarshal(s.JSON(), &wire); err != nil || wire[OffloadSummaryKey].Ref != s.Ref {
		t.Fatalf("wire form: %s", s.JSON())
	}
}

// A result longer than its preview carries its ledger reference even inline, so a
// worker can hand it on with answer_ref.
func TestExecute_LongInlineResultCarriesItsReference(t *testing.T) {
	e, _ := ledgerExec(t, &fakeHandler{result: []byte(`"` + strings.Repeat("a", 300) + `"`)}, SystemTool{Name: "t"})
	e.InlineThreshold, e.PreviewChars = 4096, 100
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "worker", ToolName: "t"})
	if out.ResultJSON == nil || !IsLedgerID(out.ResultCID) || out.Offloaded != nil {
		t.Fatalf("got %+v", out)
	}
}

func TestResultTextAndShape(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"isError":false}`)
	if got := ResultText(body); got != "a\n\nb" {
		t.Fatalf("ResultText = %q", got)
	}
	if got := ResultShape([]byte(`[1,2,3]`)); got != "JSON list of 3 items" {
		t.Fatalf("shape = %q", got)
	}
	if got := ResultText([]byte(`"plain"`)); got != "plain" {
		t.Fatalf("string = %q", got)
	}
}
