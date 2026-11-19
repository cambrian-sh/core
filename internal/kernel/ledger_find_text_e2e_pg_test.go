package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory/vault"
)

type longReportHandler struct{ body string }

func (h longReportHandler) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	return []byte(h.body), nil
}

// ENG-1761 end to end: a tool result whose text body is longer than the indexed
// 16 KiB head, recorded by the ToolExecutor into the production ledger
// composition (NewLedgerStack: Postgres store + artifact vault + Recorder +
// enforcing reader), is found by the agent's ledger_find tool on a word that sits
// only in its tail. Before, ledger_find answered count 0.
//
// Requires PG_TEST_DSN.
func TestPg_E2E_LedgerFindFindsAWordPastTheIndexedHead(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Ledger.Enabled = true
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	pool := vec.Pool()
	led := NewLedgerStack(ctx, cfg.Execution.Ledger, cfg.Execution.Content,
		LedgerStackDeps{Pool: pool, Bodies: vault.NewArtifactVault(t.TempDir()), Authorizer: domain.AllowAllAuthorizer{}})
	if led == nil {
		t.Fatal("ledger enabled but NewLedgerStack returned nil")
	}
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	agentID, convID := "eng1761-agent-"+stamp, "eng1761-conv-"+stamp
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE conversation_id = $1`, convID)
	})

	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "report_export", ClassificationTags: []string{"scope-public"}, Effects: []domain.ToolEffect{domain.EffectRead}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set(agentID, []domain.ToolGrant{{Tool: "report_export"}})
	body := strings.Repeat("filler line of an exported report\n", 1200) + "invoice INV-7731 is overdue\n" // ~40 KiB
	exec, err := domain.NewToolExecutor(domain.ToolExecutorControls{Registry: reg, Grants: grants, Authz: domain.AllowAllAuthorizer{}},
		domain.ToolExecutor{Handler: longReportHandler{body: body}, Ledger: led.Writer})
	if err != nil {
		t.Fatal(err)
	}
	prov := domain.LedgerProvenance{ConversationID: convID, TurnID: "turn-" + stamp, StepIndex: -1}
	toolCtx := domain.WithLedgerProvenance(ctx, prov)
	if out := exec.Execute(toolCtx, domain.ToolCallRequest{AgentID: agentID, ToolName: "report_export", ArgsJSON: []byte(`{}`)}); out.Denied || out.Error != "" {
		t.Fatalf("report_export: %+v", out)
	}

	var find domain.KernelTool
	for _, kt := range led.Tools {
		if kt.Spec().Name == ledger.ToolFind {
			find = kt
		}
	}
	if find == nil {
		t.Fatal("ledger stack offers no ledger_find")
	}
	callCtx := domain.WithScope(domain.WithLedgerProvenance(ctx, prov), &domain.TagPredicate{Bypass: true})
	ask := func(text string) int {
		t.Helper()
		raw, err := find.Call(callCtx, domain.KernelToolCall{AgentID: agentID,
			ArgsJSON: []byte(`{"scope":"conversation","text":"` + text + `"}`)})
		if err != nil {
			t.Fatalf("ledger_find %q: %v", text, err)
		}
		var res struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		return res.Count
	}
	if n := ask("inv-7731"); n != 1 {
		t.Fatalf("ledger_find on a word past the 16 KiB head: count %d, want 1", n)
	}
	if n := ask("not-in-the-report"); n != 0 {
		t.Fatalf("ledger_find on an absent word: count %d, want 0", n)
	}
}
