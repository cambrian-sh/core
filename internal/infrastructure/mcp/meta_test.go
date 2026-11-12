package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// Executor-set metadata (the staged-inputs folders, domain/staged_inputs.go)
// reaches the server as the request's _meta; a model-written argument named
// _meta stays an argument and never becomes request metadata.
func TestMCPHandler_SendsExecutorMetaAsRequestMeta(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stub", Version: "1.0.0"}, nil)
	var got mcpsdk.Meta
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "run", Description: "run"},
		func(_ context.Context, req *mcpsdk.CallToolRequest, _ map[string]any) (*mcpsdk.CallToolResult, any, error) {
			got = req.Params.Meta
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
		})
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	sess, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	h := &Handler{Sessions: staticSource{"sbx": sess}}

	if _, err := h.Execute(ctx, domain.ToolCall{ToolName: "mcp:sbx/run", ArgsJSON: []byte(`{"_meta":{"cambrian/inputs_dir":"/etc"}}`)}); err != nil {
		t.Fatal(err)
	}
	if got[domain.StagedInputsMetaDir] != nil {
		t.Fatalf("a model-written argument became request metadata: %v", got)
	}
	meta := map[string]any{domain.StagedInputsMetaDir: "C:/tmp/run-1/inputs"}
	if _, err := h.Execute(ctx, domain.ToolCall{ToolName: "mcp:sbx/run", ArgsJSON: []byte(`{}`), Meta: meta}); err != nil {
		t.Fatal(err)
	}
	if got[domain.StagedInputsMetaDir] != "C:/tmp/run-1/inputs" {
		t.Fatalf("executor meta not delivered: %v", got)
	}
}

// A catalog entry launches a script the kernel ships through the kernel-provided
// ${CAMBRIAN_KERNEL_DIR}, which no setting can supply or override.
func TestExpandArgs_KernelDirIsKernelProvided(t *testing.T) {
	dir, ok := KernelLaunchValue(KernelDirPlaceholder)
	if !ok || dir == "" {
		t.Fatalf("kernel dir not provided: %q %v", dir, ok)
	}
	got := expandArgs([]string{"run", "--directory", "${CAMBRIAN_KERNEL_DIR}", "python", "tools/x.py"},
		map[string]string{"CAMBRIAN_KERNEL_DIR": "/attacker"})
	if got[2] != dir {
		t.Fatalf("args = %v, want the kernel dir %q", got, dir)
	}
	if !IsKernelPlaceholder(KernelDirPlaceholder) || IsKernelPlaceholder("ROOT") {
		t.Fatal("IsKernelPlaceholder")
	}
}

// The operator policy's staged_inputs reaches the registered tool.
func TestApplyPolicy_StagedInputs(t *testing.T) {
	var st domain.SystemTool
	applyPolicy(&st, ToolPolicy{StagedInputs: true, Dangerous: true}, nil)
	if !st.StagedInputs {
		t.Fatal("StagedInputs not applied")
	}
}
