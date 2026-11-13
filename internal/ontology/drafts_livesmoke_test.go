//go:build livesmoke

package ontology

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// The agent-drafts live smoke (ADR-0149 D2): a BOOTED premium kernel (its own
// binary, port, throwaway database and data directory), driven over gRPC.
//
//	LIVESMOKE_GRPC                       kernel gRPC address
//	LIVESMOKE_DSN                        the kernel's (throwaway) database
//	LIVESMOKE_USER / LIVESMOKE_PASSWORD  the bootstrap operator
//
// A lease on a live kernel exists only inside a dispatched agent step, so the
// draft is recorded by the production Propose code from this process (same
// store, same hub persistence) — exactly the rows an agent's
// Orchestrator/ProposeOntologyChange writes — and everything after it is the
// booted kernel's: its operator plane answers, its boot-registered
// ontology_change handler applies, its reconcile loop records a denial.
//
// go test -tags livesmoke -run TestLiveSmoke_Drafts ./internal/ontology/
func TestLiveSmoke_DraftsOnABootedKernel(t *testing.T) {
	addr, dsn := os.Getenv("LIVESMOKE_GRPC"), os.Getenv("LIVESMOKE_DSN")
	if addr == "" || dsn == "" {
		t.Skip("LIVESMOKE_GRPC / LIVESMOKE_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// 1. The agent RPC is served (not UNIMPLEMENTED) and refuses a caller with no lease.
	agentCtx := metadata.AppendToOutgoingContext(ctx, "x-agent-id", "smoke-agent")
	_, err = pb.NewOrchestratorClient(conn).ProposeOntologyChange(agentCtx, &pb.ProposeOntologyChangeRequest{
		ChangeJson: `{"kinds":[{"kind":"smoke_shipment"}]}`})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("agent draft without a lease: %v (UNIMPLEMENTED would mean the drafts bridge is not wired)", err)
	}
	// 2. An agent cannot apply: the operator apply refuses a caller with no operator bearer.
	err = conn.Invoke(agentCtx, "/cambrian.premium.substrate.SubstrateLane/ApplyOntologyChange", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("agent ApplyOntologyChange: %v", err)
	}

	login, err := pb.NewOperatorConsoleClient(conn).Login(ctx, &pb.LoginRequest{
		Username: os.Getenv("LIVESMOKE_USER"), Password: os.Getenv("LIVESMOKE_PASSWORD")})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	opCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+login.GetToken())
	console := pb.NewOperatorConsoleClient(conn)

	// 3. Draft two changes as an agent would (the kernel's base, read from its table).
	base, baseVersion := liveBase(t, ctx, pool)
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: postgres.NewPgApprovalStore(pool)})
	drafts := newDrafts(dbClassifier{doc: base, version: baseVersion}, pgDraftStore{pool: pool}, hub, nil, nil)
	const ns = "livesmoke-drafts"
	agent, person := domain.AgentPrincipal("smoke-agent"), domain.UserPrincipal(login.GetPrincipal())
	ship := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "smoke_shipment", Predicates: map[string]domain.ValueSpec{"carrier": {Type: "text"}}}}}
	approve, err := drafts.Propose(ctx, agent, person, DraftProposal{Namespace: ns, Change: ship, Reason: "live smoke"})
	if err != nil {
		t.Fatal(err)
	}
	deny, err := drafts.Propose(ctx, agent, person, DraftProposal{Namespace: ns, Change: ship, ClientRef: "deny"})
	if err != nil {
		t.Fatal(err)
	}

	// 4. The operator sees them in the approvals inbox, answerable.
	list, err := console.ListApprovals(opCtx, &pb.ListApprovalsOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, a := range list.GetApprovals() {
		if (a.GetId() == approve.Draft.ApprovalID || a.GetId() == deny.Draft.ApprovalID) && a.GetKind() == domain.ApprovalKindOntologyChange && a.GetAnswerable() {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("the inbox shows %d of the 2 drafts as answerable: %+v", seen, list.GetApprovals())
	}

	// 5. The operator approves one and denies the other through the operator plane.
	if r, err := console.AnswerApproval(opCtx, &pb.AnswerApprovalOpRequest{CommandId: "smoke-approve-" + approve.Draft.ID,
		Reason: "live smoke", Id: approve.Draft.ApprovalID, Approve: true}); err != nil || r.GetRefusal() != "" {
		t.Fatalf("approve: %+v %v", r, err)
	}
	if r, err := console.AnswerApproval(opCtx, &pb.AnswerApprovalOpRequest{CommandId: "smoke-deny-" + deny.Draft.ID,
		Reason: "live smoke", Id: deny.Draft.ApprovalID, Approve: false}); err != nil || r.GetRefusal() != "" {
		t.Fatalf("deny: %+v %v", r, err)
	}

	// 6. The kernel applied the approved draft as the operator, never the agent.
	var author string
	var version int64
	if err := pool.QueryRow(ctx, `SELECT author, version FROM ontology_declarations WHERE namespace = $1`, ns).Scan(&author, &version); err != nil ||
		author != login.GetPrincipal() {
		t.Fatalf("declaration author %q (want %s): %v", author, login.GetPrincipal(), err)
	}
	if s, _, _ := (pgDraftStore{pool: pool}).getDraft(ctx, approve.Draft.ID); s.State != domain.OntologyDraftApplied || s.AppliedVersion != version {
		t.Fatalf("approved draft: %+v", s)
	}
	// 7. The kernel's reconcile loop records the denial (its interval is 30 s).
	deadline := time.Now().Add(75 * time.Second)
	for {
		s, _, _ := (pgDraftStore{pool: pool}).getDraft(ctx, deny.Draft.ID)
		if s.State == domain.OntologyDraftRejected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the denied draft is still %s", s.State)
		}
		time.Sleep(2 * time.Second)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ontology_declarations WHERE namespace = $1`, ns).Scan(&count); err != nil || count != 1 {
		t.Fatalf("the denial wrote a declaration: %d %v", count, err)
	}
	t.Logf("applied v%d as %s; denied draft rejected by the kernel's reconcile loop", version, author)
}

// liveBase reads the kernel's current boot declarations from its table.
func liveBase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (domain.OntologyDoc, int64) {
	t.Helper()
	var raw []byte
	var v int64
	if err := pool.QueryRow(ctx, `SELECT declarations, version FROM ontology_declarations WHERE namespace = '*'
		ORDER BY version DESC LIMIT 1`).Scan(&raw, &v); err != nil {
		t.Fatal(err)
	}
	var doc domain.OntologyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, v
}

// dbClassifier classifies a draft against the kernel's base as its dry run does
// (a fresh namespace has the base's declarations). It never applies: applying
// is the booted kernel's job.
type dbClassifier struct {
	doc     domain.OntologyDoc
	version int64
}

func (c dbClassifier) Apply(_ context.Context, ns string, change domain.OntologyDoc, _ string, _ bool) (domain.OntologyApplyResult, error) {
	changes := domain.ClassifyOntologyChange(c.doc, c.doc.Overlay(change))
	return domain.OntologyApplyResult{Namespace: ns, Version: c.version, Class: domain.MaxChangeClass(changes), Changes: changes}, nil
}

func (dbClassifier) apply(context.Context, string, domain.OntologyDoc, string, bool, applyCheck) (domain.OntologyApplyResult, error) {
	return domain.OntologyApplyResult{}, errors.New("the live smoke never applies in-process")
}
