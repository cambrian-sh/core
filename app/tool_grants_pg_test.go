package app

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

func TestBuildToolGrants_NoPoolIsInMemory(t *testing.T) {
	ctx := context.Background()
	g, err := buildToolGrants(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ReplaceGrants(ctx, "a", []domain.ToolGrant{{Tool: "propose_knowledge"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.GrantsFor(ctx, "a"); len(got) != 1 {
		t.Fatalf("in-memory grants = %+v", got)
	}
}

// grantsKernel is the slice of the composition one boot builds for tool grants:
// the grants view (buildToolGrants), the profile grant sync over it, the
// executor, and the operator plane's command sources (durable audit + grants).
type grantsKernel struct {
	pool   *pgxpool.Pool
	grants *domain.LayeredGrantsStore
	exec   *domain.ToolExecutor
	op     *operator.Service
}

const e2eProfileAgent = "eng1116-assistant"

func bootGrantsKernel(t *testing.T, dsn string) *grantsKernel {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	grants, err := buildToolGrants(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	reg := domain.NewInMemoryToolRegistry()
	for _, n := range []string{"propose_knowledge", "propose_ontology_change", "mcp:fs/read_file", "mcp:shell/run"} {
		reg.Register(domain.SystemTool{Name: n})
	}
	profiles := []profileAgent{{ID: e2eProfileAgent, Allow: domain.ToolAllowlist{"mcp:fs/read_*", "propose_*"}}}
	newProfileGrantSync(grants.Profile(), reg.All, profiles, true).Sync(ctx, true)
	exec := newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Ceiling: profileToolCeiling(profiles)},
		domain.ToolExecutor{MCPHandler: okHandler{}, InlineThreshold: 1 << 20})
	audit, err := postgres.NewPgAuditStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	op := operatortest.New(t, nil, func(d *operator.Deps, _ *operator.Options) {
		d.Audit, d.Grants = audit, grants
	})
	return &grantsKernel{pool: pool, grants: grants, exec: exec, op: op}
}

func (k *grantsKernel) allowed(agent, tool string) bool {
	r := k.exec.Execute(context.Background(), domain.ToolCallRequest{AgentID: agent, ToolName: tool, ArgsJSON: []byte(`{}`)})
	return !r.Denied
}

func (k *grantsKernel) setGrant(t *testing.T, cmd, agent, tool string, granted bool) {
	t.Helper()
	ctx := context.Background() // the operator interceptor is not under test; SetToolGrant reads no principal it requires
	if _, err := k.op.SetToolGrant(ctx, &pb.SetToolGrantRequest{CommandId: cmd, Reason: "ENG-1116 e2e",
		AgentId: agent, ToolName: tool, Granted: granted}); err != nil {
		t.Fatalf("SetToolGrant(%s %s %v): %v", agent, tool, granted, err)
	}
}

// End to end against a migrated Postgres (PG_TEST_DSN, 0052): an operator grant
// set through the OperatorConsole RPC survives a restart of the composition and
// is enforced by the executor; a revoke survives too; profile allowlist grants
// are re-derived, never persisted; a store error denies the call.
func TestPg_ToolGrants_OperatorGrantSurvivesRestart(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t) // migrates the database itself (ENG-2100)
	ctx := context.Background()
	k1 := bootGrantsKernel(t, dsn)
	if !postgres.NewPgToolGrantStore(k1.pool).Ready(ctx) {
		t.Fatal("tool_grants not ready on the migrated test database")
	}
	clean := func() {
		_, _ = k1.pool.Exec(context.Background(), `DELETE FROM tool_grants WHERE agent_id LIKE 'eng1116-%'`)
		_, _ = k1.pool.Exec(context.Background(), `DELETE FROM operator_audit WHERE command_id LIKE 'eng1116-%'`)
	}
	clean()
	t.Cleanup(clean)
	const worker = "eng1116-worker"

	if k1.allowed(worker, "propose_knowledge") {
		t.Fatal("an ungranted agent may call propose_knowledge")
	}
	k1.setGrant(t, "eng1116-cmd-1", worker, "propose_knowledge", true)
	k1.setGrant(t, "eng1116-cmd-2", worker, "propose_ontology_change", true)
	if !k1.allowed(worker, "propose_knowledge") {
		t.Fatal("granted call denied before restart")
	}
	if !k1.allowed(e2eProfileAgent, "mcp:fs/read_file") || k1.allowed(e2eProfileAgent, "mcp:shell/run") {
		t.Fatal("profile grants wrong before restart")
	}
	var profileRows int
	if err := k1.pool.QueryRow(ctx, `SELECT count(*) FROM tool_grants WHERE agent_id = $1`, e2eProfileAgent).Scan(&profileRows); err != nil {
		t.Fatal(err)
	}
	if profileRows != 0 {
		t.Fatalf("profile allowlist grants were persisted: %d rows", profileRows)
	}

	// Restart: a new pool, grants view, profile sync, executor and operator plane.
	k2 := bootGrantsKernel(t, dsn)
	if !k2.allowed(worker, "propose_knowledge") || !k2.allowed(worker, "propose_ontology_change") {
		t.Fatal("operator grants were lost on restart")
	}
	if k2.allowed(worker, "mcp:shell/run") {
		t.Fatal("restart widened the worker's grants")
	}
	if !k2.allowed(e2eProfileAgent, "mcp:fs/read_file") || !k2.allowed(e2eProfileAgent, "propose_knowledge") {
		t.Fatal("profile grants not re-derived after restart")
	}
	// The replayed command_id is a dedupe across the restart (durable audit).
	opCtx := ctx
	if ack, err := k2.op.SetToolGrant(opCtx, &pb.SetToolGrantRequest{CommandId: "eng1116-cmd-1", Reason: "replay",
		AgentId: worker, ToolName: "propose_knowledge", Granted: true}); err != nil || !ack.GetDeduped() {
		t.Fatalf("replay across restart: ack=%+v err=%v", ack, err)
	}

	// Revoke, restart: still revoked.
	k2.setGrant(t, "eng1116-cmd-3", worker, "propose_knowledge", false)
	k3 := bootGrantsKernel(t, dsn)
	if k3.allowed(worker, "propose_knowledge") {
		t.Fatal("revoke was lost on restart")
	}
	if !k3.allowed(worker, "propose_ontology_change") {
		t.Fatal("revoking one tool dropped another")
	}

	// Store error → the grant check denies (fail closed).
	k3.pool.Close()
	if k3.allowed(worker, "propose_ontology_change") {
		t.Fatal("a call was allowed while the grants store is unreachable")
	}
}
