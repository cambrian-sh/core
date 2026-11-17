package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// ADR-0157 S4 end to end — artifacts leave an agent only through the kernel —
// through the REAL paths with a REAL Python SDK agent:
//
//	app.wireLedger → kernel.NewLedgerStack (Postgres ledger_entries + vault, the
//	  ADR-0157 Q6 import of a retired bbolt artifact bucket, the collector wired
//	  into the InstanceManager)
//	InstanceManager.GetOrBootInstance → startSandboxed (per-instance scratch,
//	  CAMBRIAN_AGENT_OUT) → the agent's SDK channel (per-instance token, S3)
//	  → the production agent-credential interceptor (stamps principal AND
//	  instance) → network.Server.UploadArtifact (lease from x-lease-id)
//	EvictInstance → the process's one Wait → cleanup: out/ collected into the
//	  ledger BEFORE the scratch is released.
//
// Asserted (ADR-0157 S4 gate):
//   - an upload is a ledger artifact entry written by the agent AND its instance
//     (verified context), in the run/session/step of the lease it presented;
//   - the same bytes uploaded by a second agent are a second entry with that
//     agent's own tags; each reads its own, a receiver outside scope gets
//     found=false;
//   - out/ files appear as ledger artifact entries attributed to the instance,
//     derived from the step's tool results (DerivedFrom, source refs) and
//     classified at write; a file over max_file_bytes is not collected; the
//     scratch is gone after exit;
//   - the retired bbolt artifact record is imported with its tags (Q6) and the
//     bucket dropped.
//
// Needs a Python with the agent SDK and a migrated Postgres:
//
//	ARTIFACT_E2E_PY   the interpreter (e.g. sdk/.venv/Scripts/python.exe)
//	PG_TEST_DSN       a migrated, disposable database (through 0053)
//
// Linux runs the sandbox as bubblewrap, Windows as low integrity + a job object.
func TestE2E_ArtifactsLeaveOnlyThroughTheKernel(t *testing.T) {
	py, dsn := os.Getenv("ARTIFACT_E2E_PY"), os.Getenv("PG_TEST_DSN")
	if py == "" || dsn == "" {
		t.Skip("set ARTIFACT_E2E_PY and PG_TEST_DSN to run the artifact e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // registered first: closes after every teardown below
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE run_id LIKE 's4e-%' OR session_id LIKE 's4e-%' OR writer LIKE 'agent:s4_%'`)
	}
	clean()
	t.Cleanup(clean)

	base, err := os.MkdirTemp("", "s4e")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(base, 0o755) // a dropped uid (Linux root) must reach code and scratch
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "s4_probe.py"), []byte(artifactProbePy), 0o644); err != nil {
		t.Fatal(err)
	}

	// The retired bbolt artifact bucket with one record whose bytes are in the vault.
	av := vault.NewArtifactVault(filepath.Join(base, "vault"))
	legacyHash, err := av.Store([]byte("legacy upload bytes"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "agents.db")
	seedLegacyArtifactBucket(t, dbPath, storage.ArtifactRecord{Hash: legacyHash, ContentType: "text/plain",
		SizeBytes: 19, SessionID: "s4e-legacy-sess", StepIndex: 2, Tags: []string{"team_a"}})
	store, err := storage.NewBBoltAdapterNoScan(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	legacy := kernel.NewAgentRepoDecorator(store)

	reg := metabolism.NewInMemoryRegistry()
	probe := &domain.AgentDefinition{ID: "s4_probe", Runtime: domain.RuntimePython, ExecPath: "s4_probe.py", Dir: code}
	other := &domain.AgentDefinition{ID: "s4_other", Runtime: domain.RuntimePython, ExecPath: "s4_probe.py", Dir: code}
	reg.SetAgent(*probe)
	reg.SetAgent(*other)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentmgr.NewAgentManager(reg, py, lis.Addr().String(), nil)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(60 * time.Second)
	kernelDir := filepath.Join(base, "kernel")
	if err := os.MkdirAll(filepath.Join(kernelDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(base, "w")
	im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		WorkRoot: workRoot, KernelDir: kernelDir, KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
	})})

	exec := config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
		Ledger:  config.LedgerConfig{Enabled: true, Artifacts: config.LedgerArtifactsConfig{MaxFileBytes: 1024}},
	}
	gw := network.NewLLMGateway(exec, llm.NewSpendMeter(0, nil, nil))
	az := s4Authorizer{}
	srvImpl := &network.Server{LLMGateway: gw, Manager: mgr, Authz: az, ExecCfg: exec}
	if sw := wireLedger(ctx, exec.Ledger, exec.Content, pool, av, legacy, az, false, srvImpl,
		network.NewTurnDelegations(), nil); sw == nil {
		t.Fatal("the run ledger did not come up (is PG_TEST_DSN migrated through 0053?)")
	}

	// Q6: the legacy record is a ledger artifact now, with its own tags; the bucket is gone.
	legacyRows, err := srvImpl.ListStepArtifacts(agentMD("s4_probe"), &pb.ListStepArtifactsRequest{SessionId: "s4e-legacy-sess", StepIndex: 2})
	if err != nil || len(legacyRows.Artifacts) != 1 || strings.Join(legacyRows.Artifacts[0].Tags, ",") != "team_a" {
		t.Fatalf("legacy artifact after the Q6 import: %+v %v", legacyRows, err)
	}
	if recs, err := store.ListLegacyArtifactRecords(); err != nil || recs != nil {
		t.Fatalf("the retired bucket survived the import: %v %v", recs, err)
	}
	if got, err := srvImpl.GetArtifact(agentMD("s4_other"), &pb.GetArtifactRequest{Hash: legacyRows.Artifacts[0].Hash}); err != nil || got.Found {
		t.Fatalf("an out-of-scope agent read the imported legacy artifact: %+v %v", got, err)
	}

	verifier, err := authz.NewAgentCredentialVerifier(nil, im.Tokens(), kernelSpawnedAgent(reg), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), authz.UnaryAgentAuthInterceptor(verifier, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), authz.StreamAgentAuthInterceptor(verifier, nil)),
	)
	pb.RegisterOrchestratorServer(srv, srvImpl)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		_ = im.KillAllAgents(context.Background())
		srv.Stop()
	})

	// The step the probe works on: a lease bound to it, and one tool result already
	// recorded in that step (as the ToolExecutor would) that quotes a document.
	lease, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{AgentID: "s4_probe", RunID: "s4e-run", StepIndex: 0, SessionID: "s4e-sess"})
	toolRes, err := srvImpl.Ledger.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult,
		Provenance: domain.LedgerProvenance{RunID: "s4e-run", StepIndex: 0, SessionID: "s4e-sess"},
		ToolName:   "crm_get", Writer: domain.AgentPrincipal("s4_probe"), SourceRefs: []string{"s4e-doc"},
		ContentType: "application/json", Body: []byte(`{"customer":"C-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "lease.txt"), []byte(lease), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. The probe uploads with its lease and writes out/; the other agent uploads
	//    the same bytes with no lease.
	inst, err := im.GetOrBootInstance(ctx, probe, "")
	if err != nil {
		t.Fatalf("boot probe: %v", err)
	}
	upID := readProbeSide(t, inst, ".up")
	instOther, err := im.GetOrBootInstance(ctx, other, "")
	if err != nil {
		t.Fatalf("boot other: %v", err)
	}
	otherID := readProbeSide(t, instOther, ".up")
	if !domain.IsLedgerID(upID) || !domain.IsLedgerID(otherID) || upID == otherID {
		t.Fatalf("uploads: probe=%q other=%q (want two distinct ledger ids)", upID, otherID)
	}
	up := rawEntry(t, ctx, pool, upID)
	if up.writer != "agent:s4_probe" || up.instance != inst.ID || up.run != "s4e-run" || up.session != "s4e-sess" ||
		!strings.Contains(up.tags, "team_a") || strings.Contains(up.tags, "team_b") {
		t.Fatalf("probe upload provenance: %+v (instance want %s)", up, inst.ID)
	}
	ot := rawEntry(t, ctx, pool, otherID)
	if ot.writer != "agent:s4_other" || ot.instance != instOther.ID || !strings.Contains(ot.tags, "team_b") {
		t.Fatalf("other upload provenance: %+v", ot)
	}
	if got, err := srvImpl.GetArtifact(agentMD("s4_other"), &pb.GetArtifactRequest{Hash: upID}); err != nil || got.Found {
		t.Fatalf("a receiver outside scope read the probe's upload: %+v %v", got, err)
	}
	if got, err := srvImpl.GetArtifact(agentMD("s4_probe"), &pb.GetArtifactRequest{Hash: upID}); err != nil || !got.Found ||
		string(got.Content) != "same bytes" {
		t.Fatalf("the probe cannot read its own upload: %+v %v", got, err)
	}

	// 2. The probe exits; its out/ is collected before the scratch goes.
	scratch := probeScratch(t, workRoot, "s4_probe")
	im.EvictInstance(inst.ID)
	var outs []rawRow
	deadline := time.Now().Add(30 * time.Second)
	for {
		outs = rawEntries(t, ctx, pool, `SELECT id FROM ledger_entries WHERE tool_name = 'agent_out' AND writer = 'agent:s4_probe' ORDER BY args::text`)
		if len(outs) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(outs) != 2 {
		t.Fatalf("collected out/ entries = %d, want 2 (report.csv, sub/notes.txt; big.bin over the cap)", len(outs))
	}
	for _, o := range outs {
		if o.kind != "artifact" || o.instance != inst.ID || o.run != "s4e-run" || o.step != 0 || o.session != "s4e-sess" ||
			!strings.Contains(o.derived, toolRes.ID) || !strings.Contains(o.refs, "s4e-doc") || !strings.Contains(o.tags, "team_a") {
			t.Fatalf("out/ entry provenance: %+v (want instance %s, derived from %s)", o, inst.ID, toolRes.ID)
		}
	}
	if !strings.Contains(outs[0].args, "out/report.csv") || !strings.Contains(outs[1].args, "out/sub/notes.txt") {
		t.Fatalf("out/ entries: %s / %s", outs[0].args, outs[1].args)
	}
	if got, err := srvImpl.GetArtifact(agentMD("s4_probe"), &pb.GetArtifactRequest{Hash: outs[0].id}); err != nil || !got.Found ||
		string(got.Content) != "a,b\n1,2\n" {
		t.Fatalf("collected report.csv: %+v %v", got, err)
	}
	for time.Now().Before(deadline) {
		if _, err := os.Stat(scratch); os.IsNotExist(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("the instance scratch %s survived the exit: %v", scratch, err)
	}
}

// s4Authorizer: s4_probe reads and writes team_a, s4_other team_b.
type s4Authorizer struct{ domain.AllowAllAuthorizer }

func s4Team(id string) string {
	if id == "s4_other" {
		return "team_b"
	}
	return "team_a"
}

func (s4Authorizer) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return &domain.TagPredicate{RequiredTags: []string{s4Team(p.ID)}}, domain.AccessDecision{Allowed: true, Principal: p, Surface: s, Reason: domain.ReasonAllowed}
}

func (s4Authorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, _ []string) ([]string, domain.AccessDecision) {
	return []string{s4Team(p.ID)}, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func agentMD(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", id))
}

func seedLegacyArtifactBucket(t *testing.T, path string, rec storage.ArtifactRecord) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b, _ := json.Marshal(rec)
	if err := db.Update(func(tx *bbolt.Tx) error {
		bk, err := tx.CreateBucketIfNotExists([]byte("artifacts"))
		if err != nil {
			return err
		}
		return bk.Put([]byte(rec.Hash), b)
	}); err != nil {
		t.Fatal(err)
	}
}

type rawRow struct {
	id, kind, writer, instance, run, session, tags, derived, refs, args string
	step                                                                int
}

func rawEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) rawRow {
	t.Helper()
	rows := rawEntries(t, ctx, pool, `SELECT id FROM ledger_entries WHERE id = '`+id+`'`)
	if len(rows) != 1 {
		t.Fatalf("ledger entry %s: %d rows", id, len(rows))
	}
	return rows[0]
}

func rawEntries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idQuery string) []rawRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, kind, writer, instance_id, run_id, session_id, step_index, tags::text,
		derived_from::text, source_refs::text, coalesce(args::text, '') FROM ledger_entries WHERE id IN (`+idQuery+`) ORDER BY args::text`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.kind, &r.writer, &r.instance, &r.run, &r.session, &r.step, &r.tags, &r.derived, &r.refs, &r.args); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// probeScratch is the probe instance's scratch directory under the work root.
func probeScratch(t *testing.T, workRoot, agentID string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(workRoot, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.HasPrefix(d.Name(), agentID+"-") && found == "" {
			found = p
			return filepath.SkipDir
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no scratch directory for %s under %s", agentID, workRoot)
	}
	return found
}

func readProbeSide(t *testing.T, inst *domain.Instance, suffix string) string {
	t.Helper()
	p := inst.SocketPath + suffix
	deadline := time.Now().Add(60 * time.Second)
	for {
		b, err := os.ReadFile(p)
		if err == nil && len(b) > 0 {
			s := strings.TrimSpace(string(b))
			if strings.HasPrefix(s, "ERROR") {
				t.Fatalf("probe %s: %s", inst.AgentID, s)
			}
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s wrote no %s side channel: %v", inst.AgentID, suffix, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// artifactProbePy: spawned by the kernel it serves --socket, writes three files
// to its out/ folder (CAMBRIAN_AGENT_OUT; big.bin over the 1 KiB cap), uploads
// "same bytes" through the SDK channel — with the lease in lease.txt when it is
// s4_probe — and writes the returned artifact reference to <socket>.up.
const artifactProbePy = `
import os, sys
from concurrent import futures
import grpc
from cambrian_agent_sdk.channel import build_channel
from cambrian_agent_sdk.clients import ArtifactManager
from cambrian_agent_sdk._proto import cambrian_pb2, cambrian_pb2_grpc

def arg(name):
    a = sys.argv
    return a[a.index(name) + 1] if name in a and a.index(name) + 1 < len(a) else ""

addr, sock, agent = arg("--substrate-addr"), arg("--socket"), os.environ.get("CAMBRIAN_AGENT_ID", "")
server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
server.add_insecure_port("unix:" + sock)
server.start()
try:
    out = os.environ["CAMBRIAN_AGENT_OUT"]
    os.makedirs(os.path.join(out, "sub"), exist_ok=True)
    with open(os.path.join(out, "report.csv"), "w", newline="") as f:
        f.write("a,b\n1,2\n")
    with open(os.path.join(out, "sub", "notes.txt"), "w") as f:
        f.write("notes")
    with open(os.path.join(out, "big.bin"), "wb") as f:
        f.write(b"x" * 2048)
    stub = cambrian_pb2_grpc.OrchestratorStub(build_channel(addr))
    if agent == "s4_probe":
        lease = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "lease.txt")).read().strip()
        ref = stub.UploadArtifact(cambrian_pb2.UploadArtifactRequest(content=b"same bytes", content_type="text/plain",
            session_id="forged", step_index=9), metadata=[("x-lease-id", lease)], timeout=30).hash
    else:
        ref = ArtifactManager(stub=stub).save(b"same bytes", content_type="text/plain")
    msg = ref
except Exception as e:
    msg = "ERROR " + repr(e)
with open(sock + ".up", "w") as f:
    f.write(msg)
server.wait_for_termination()
`
