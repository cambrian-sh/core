package network

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// End to end over real Postgres (PG_TEST_DSN; migrations 0040 + 0041), the
// production pieces composed the way app.go composes them: an agent drafts
// through Orchestrator/ProposeOntologyChange with its lease → the
// ontology_change approval is raised on the real hub (Postgres store, shipped
// policy) → a person answers through the operator plane's AnswerApproval RPC →
// the next substrate write validates against the approved declarations.
func TestPg_E2E_AgentDraftApprovedByAPersonReachesTheNextWrite(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ns = "e2e-drafts"
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`TRUNCATE ontology_draft_events, ontology_drafts, ontology_declarations`,
			`DELETE FROM approvals WHERE kind = 'ontology_change'`,
			`DELETE FROM knowledge_items WHERE namespace_id = 'e2e-drafts'`,
		} {
			_, _ = pool.Exec(bg, q)
		}
	}
	clean()
	t.Cleanup(clean)

	seed := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped"}}, "total": {Type: "number"}}}, domain.MemoryFactKindSpec()}}
	onto, err := ontology.Load(ctx, pool, seed, []domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: postgres.NewPgApprovalStore(pool)})
	audit := operator.NewInMemoryAuditStore()
	drafts, err := ontology.NewDrafts(pool, onto, hub, func(ctx context.Context, e domain.AuditEntry) error {
		_, err := audit.Record(ctx, e)
		return err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, gw := newLeaseServer(t)
	s.OntologyDrafts = drafts
	// Restricted tool grants (the default, tools_unrestricted=false): the agent
	// may draft only once an operator grants it the verb
	// (T-20261113-agent-proposal-rpcs-proposeknowledge).
	grants := domain.NewInMemoryGrantsStore()
	s.ToolExecutor = &domain.ToolExecutor{Grants: grants}
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	op := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	op.SetCommandSources(audit, domain.NewInMemoryGrantsStore())
	op.SetSteeringSources(operator.NewExecutionControlHub(), hub)
	store := postgres.NewPgKnowledgeStore(pool, onto)

	order := domain.KnowledgeItem{NamespaceID: ns, Kind: "order", EntityID: "order/1", AssertedBy: "user:clerk",
		AssertedAt: time.Now().UTC(), SourceRef: "order/1|lost", Values: []domain.StatementValue{{Predicate: "status", Type: "text", Text: "lost"}}}
	if _, _, err := store.PutItem(ctx, order); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("a status outside the enum must be refused before the change: %v", err)
	}

	// The agent drafts with its lease — refused until it holds the verb's grant.
	lease := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r", AgentID: "planner", ConversationID: "conv-of-alice"})
	if _, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), &pb.ProposeOntologyChangeRequest{LeaseId: string(lease), Namespace: ns,
		ChangeJson: `{"kinds":[{"kind":"order","predicates":{"status":{"type":"text","enum":["open","shipped","lost"]},"total":{"type":"number"}}}]}`}); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), domain.VerbProposeOntologyChange) {
		t.Fatalf("an agent without the grant must be refused: %v", err)
	}
	if n := len(mustList(t, hub)); n != 0 {
		t.Fatalf("a refused draft raised %d approvals", n)
	}
	grants.Set("planner", []domain.ToolGrant{{Tool: domain.VerbProposeOntologyChange}})
	resp, err := s.ProposeOntologyChange(agentPlaneCtx("planner"), &pb.ProposeOntologyChangeRequest{LeaseId: string(lease), Namespace: ns,
		ChangeJson: `{"kinds":[{"kind":"order","predicates":{"status":{"type":"text","enum":["open","shipped","lost"]},"total":{"type":"number"}}}]}`,
		Reason:     "orders get lost in transit"})
	if err != nil || !resp.GetAccepted() || resp.GetState() != "pending" || resp.GetClass() != "additive" || resp.GetApprovalState() != "pending" {
		t.Fatalf("draft: %+v %v", resp, err)
	}
	// Raised to the right person: the shipped policy routes ontology_change to an
	// operator; the beneficiary (alice, not an operator) cannot answer it.
	ap, err := hub.Get(ctx, resp.GetApprovalId())
	if err != nil || ap.Kind != domain.ApprovalKindOntologyChange || ap.Object != resp.GetDraftId() || ap.AgentID != "planner" ||
		ap.Beneficiary != domain.UserPrincipal("alice") || ap.RuleSummary() != "operator" {
		t.Fatalf("approval: %+v %v", ap, err)
	}
	// The approver reads the full draft by the approval's object (the seam the
	// substrate plugin serves as GetOntologyDraft): the whole change, its class
	// against the base version, the proposer, the old spec and a clean dry run
	// (T-20261113-approvers-of-an-ontology-change-see-only-a-200).
	view, found, err := drafts.InspectDraft(ctx, ap.Object)
	if err != nil || !found || view.Draft.Proposer != "agent:planner" || view.Draft.Reason != "orders get lost in transit" ||
		view.Draft.BaseVersion != 1 || view.Draft.Class != domain.ChangeAdditive || len(view.Draft.Change.Kinds) != 1 ||
		len(view.Draft.Change.Kinds[0].Predicates["status"].Enum) != 3 || len(view.Current.Kinds) != 1 ||
		len(view.Current.Kinds[0].Predicates["status"].Enum) != 2 || view.DryRun == nil || view.DryRunRefusal != "" {
		t.Fatalf("approver's view: %+v found=%v %v", view, found, err)
	}
	// The operator plane's own authentication: the bearer token resolves the person.
	auth := operator.UnaryAuthInterceptor(tokenAuth{"tok-alice": {Principal: domain.UserPrincipal("alice"), Roles: []string{"member"}},
		"tok-bob": {Principal: domain.UserPrincipal("bob"), Roles: []string{"operator"}}}, nil, nil)
	answer := func(token, command string) (*pb.AnswerApprovalOpResponse, error) {
		md := metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
		out, err := auth(md, &pb.AnswerApprovalOpRequest{CommandId: command, Reason: "reviewed", Id: ap.ID, Approve: true},
			&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/AnswerApproval"},
			func(c context.Context, req any) (any, error) {
				return op.AnswerApproval(c, req.(*pb.AnswerApprovalOpRequest))
			})
		if err != nil {
			return nil, err
		}
		return out.(*pb.AnswerApprovalOpResponse), nil
	}
	if r, err := answer("tok-alice", "c-alice"); err == nil && r.GetRefusal() == "" || onto.Current(ns).Version != 1 {
		t.Fatalf("the beneficiary (not an operator) answered: %+v %v", r, err)
	}
	// An operator approves through the operator plane.
	r, err := answer("tok-bob", "c-bob")
	if err != nil || r.GetRefusal() != "" || r.GetState() != "approved" {
		t.Fatalf("operator answer: %+v %v", r, err)
	}
	v := onto.Current(ns).Version
	var author string
	if err := pool.QueryRow(ctx, `SELECT author FROM ontology_declarations WHERE namespace = $1 AND version = $2`, ns, v).Scan(&author); err != nil ||
		author != "user:bob" {
		t.Fatalf("declaration v%d author %q (want the approver user:bob) %v", v, author, err)
	}
	// The next write validates against the approved declarations.
	id, _, err := store.PutItem(ctx, order)
	if err != nil {
		t.Fatalf("write after approval: %v", err)
	}
	var dv int64
	if err := pool.QueryRow(ctx, `SELECT decl_version FROM knowledge_items WHERE id = $1`, string(id)).Scan(&dv); err != nil || dv != v {
		t.Fatalf("decl_version %d, want %d (%v)", dv, v, err)
	}
	// Every transition is in the operator audit: drafted, answered, applied.
	entries, err := audit.Query(ctx, domain.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.ActionType+"@"+e.Actor)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"ontology_draft_pending@agent:planner", "answer_approval@bob", "ontology_draft_applied@user:bob"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("audit lacks %s: %v", want, kinds)
		}
	}

	// A breaking draft is refused with the reason; nothing is raised.
	before := len(mustList(t, hub))
	resp, err = s.ProposeOntologyChange(agentPlaneCtx("planner"), &pb.ProposeOntologyChangeRequest{LeaseId: string(lease), Namespace: ns,
		ChangeJson: `{"kinds":[{"kind":"order","predicates":{"status":{"type":"number"},"total":{"type":"number"}}}]}`})
	if err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "breaking") ||
		!strings.Contains(resp.GetDetail(), "kind order.status: value type text -> number") || len(mustList(t, hub)) != before {
		t.Fatalf("breaking draft: %+v %v", resp, err)
	}

	// The agent plane has no way to apply: Orchestrator carries no apply RPC.
	if _, has := reflect.TypeOf((*pb.OrchestratorServer)(nil)).Elem().MethodByName("ApplyOntologyChange"); has {
		t.Fatal("the agent plane must not expose ApplyOntologyChange")
	}
}

func mustList(t *testing.T, hub *domain.AddressedHub) []domain.ApprovalItem {
	t.Helper()
	items, err := hub.List(context.Background(), domain.ApprovalQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// tokenAuth is a fixed bearer-token → identity table (domain.Authenticator).
type tokenAuth map[string]domain.Identity

func (a tokenAuth) Resolve(_ context.Context, token string) (domain.Identity, error) {
	id, ok := a[token]
	if !ok {
		return domain.Identity{}, domain.ErrUnauthenticated
	}
	return id, nil
}

func (tokenAuth) Revoke(context.Context, string) error { return nil }
