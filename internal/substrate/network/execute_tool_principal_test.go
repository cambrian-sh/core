package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// captureToolOutput records what the executor fed to the memory layer, along
// with the principal the write-side chokepoints would see on that call's context.
type captureToolOutput struct {
	recs  []domain.ToolOutputRecord
	prins []domain.PrincipalRef
}

func (c *captureToolOutput) RecordToolOutput(ctx context.Context, rec domain.ToolOutputRecord) error {
	c.recs = append(c.recs, rec)
	c.prins = append(c.prins, domain.PrincipalFromContext(ctx))
	return nil
}

type staticToolHandler struct{ result []byte }

func (h staticToolHandler) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	return h.result, nil
}

// The ExecuteTool RPC stamps the x-agent-id principal onto the call context and
// the executor passes the tool's own classification tags on the output record,
// so the LTM write is classified from principal + tool domain — not committed
// under a zero principal with a nil hint.
func TestExecuteTool_StampsPrincipalAndClassificationTags(t *testing.T) {
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{
		Name:               "write_file",
		ClassificationTags: []string{"filesystem"},
		DataWriteKinds:     []string{"filesystem"},
	})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("agent-a", []domain.ToolGrant{{Tool: "write_file", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	rec := &captureToolOutput{}
	srv := &Server{ToolExecutor: &domain.ToolExecutor{
		Registry:   reg,
		Grants:     grants,
		Handler:    staticToolHandler{result: []byte(`{"ok":1}`)},
		ToolOutput: rec,
	}}

	resp, err := srv.ExecuteTool(ctxWithAgentID("agent-a"), &pb.ExecuteToolRequest{
		ToolName: "write_file", ArgsJson: `{"path":"a.md"}`,
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if resp.Denied || resp.Error != "" {
		t.Fatalf("call must succeed: %+v", resp)
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one recorded output, got %d", len(rec.recs))
	}
	if want := domain.AgentPrincipal("agent-a"); rec.prins[0] != want {
		t.Errorf("the record's context must carry the caller principal; got %+v", rec.prins[0])
	}
	if got := rec.recs[0].ClassificationTags; len(got) != 1 || got[0] != "filesystem" {
		t.Errorf("the record must carry the tool's classification tags; got %v", got)
	}
}

// An anonymous caller (no x-agent-id) stamps nothing: a zero principal stays
// distinguishable from an authenticated one, and the authorizer decides what
// that means.
func TestExecuteTool_AnonymousCallerStampsNoPrincipal(t *testing.T) {
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "write_file", DataWriteKinds: []string{"filesystem"}})
	rec := &captureToolOutput{}
	srv := &Server{ToolExecutor: &domain.ToolExecutor{
		Registry:   reg,
		Grants:     domain.NewInMemoryGrantsStore(),
		Handler:    staticToolHandler{result: []byte(`{"ok":1}`)},
		ToolOutput: rec,
	}}

	// Anonymous ⇒ denied at the grant (fail-closed), so nothing is recorded and
	// nothing was stamped along the way.
	resp, err := srv.ExecuteTool(context.Background(), &pb.ExecuteToolRequest{ToolName: "write_file"})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("anonymous caller must be denied, got %+v", resp)
	}
	if len(rec.recs) != 0 {
		t.Errorf("a denied call must not reach the recorder, got %d records", len(rec.recs))
	}
}
