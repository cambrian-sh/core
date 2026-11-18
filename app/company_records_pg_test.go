package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/records"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2117 end to end in process: company_records wired by the boot function
// (wireCompanyRecords, default config) over the real substrate (NewSubstrate on a
// migrated Postgres), called through the real ToolExecutor.Execute — the path an
// agent's ExecuteTool takes — for a chat agent acting for different people. The
// decision point is a stand-in that answers ReadFilter by the conversation owner
// on ctx, the way premium's ADR-0091 option C does for a first-party agent
// (ENG-2107); every row test is the real SQL fence (evidence classification and
// parties). Allowed people get the order; everyone else gets "not found" and not
// one byte of it.

const crSource = "eng2117-company-records-test"

// personAuthorizer resolves the read predicate of the person the agent acts for.
type personAuthorizer struct {
	domain.AllowAllAuthorizer
	byOwner map[string]*domain.TagPredicate
}

func (a personAuthorizer) ReadFilter(ctx context.Context, _ domain.PrincipalRef, _ domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if p, ok := a.byOwner[domain.ConversationOwnerFromContext(ctx)]; ok {
		return p, domain.AccessDecision{Allowed: true}
	}
	// The agent for itself: its own closure forbids every closed tag.
	return &domain.TagPredicate{ForbiddenTags: []string{"scope-orders", "scope-quality"}}, domain.AccessDecision{Allowed: true}
}

func crClean(pool *pgxpool.Pool) {
	c := context.Background()
	for _, q := range []string{
		`DELETE FROM event_roles WHERE event_id IN (SELECT id FROM events WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = $1))`,
		`DELETE FROM events WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = $1)`,
		`DELETE FROM observations WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = $1)`,
		`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = $1)`,
		`DELETE FROM evidence WHERE source_id = $1`,
	} {
		_, _ = pool.Exec(c, q, crSource)
	}
}

func TestPg_CompanyRecords_ThroughTheToolExecutorForEachPerson(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	crClean(pool)
	t.Cleanup(func() { crClean(pool) })
	rels, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	// No object kinds declared (the world's ingresses declare none): GetObject
	// answers not found and the tool reads observations and events only.
	kinds, err := domain.NewKindRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := NewSubstrate(pool, kinds, rels)

	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	evidence := func(key string, tags, parties []string) domain.EvidenceID {
		id, _, err := sub.Evidence.Insert(ctx, domain.Evidence{NamespaceID: "default", SourceID: crSource, SourceKey: key,
			IngestedAt: at, ContentHash: domain.CID("sha256:eng2117-" + key), Classification: tags, Parties: parties})
		if err != nil {
			t.Fatalf("evidence %s: %v", key, err)
		}
		return id
	}
	observe := func(ev domain.EvidenceID, entity, pred, text string) {
		if _, err := sub.Events.RecordObservation(ctx, domain.Observation{NamespaceID: "default", EntityID: entity, Predicate: pred,
			Value: domain.StatementValue{Type: "text", Text: text}, OccurredAt: at, Confidence: 1, EvidenceID: ev,
			SourceRef: crSource + ":" + entity + ":" + pred}); err != nil {
			t.Fatalf("observation: %v", err)
		}
	}
	event := func(ev domain.EvidenceID, typ string, when time.Time, roles ...domain.EventRole) {
		if _, _, err := sub.Events.RecordEvent(ctx, domain.Event{NamespaceID: "default", Type: typ, OccurredAt: when,
			EvidenceID: ev, SourceRef: crSource + ":" + typ + ":" + string(ev), Roles: roles}); err != nil {
			t.Fatalf("event: %v", err)
		}
	}
	orderParties := []string{"cus:CUS-T1", "emp:EMP-T1"}
	placed := evidence("ORD-T1-placed", []string{"scope-orders"}, orderParties)
	shipped := evidence("ORD-T1-shipped", []string{"scope-orders"}, orderParties)
	observe(shipped, "ORD-T1", "status", "shipped_eng2117")
	event(placed, "order_placed_eng2117", at.Add(-48*time.Hour), domain.EventRole{Role: "order", EntityID: "ORD-T1"}, domain.EventRole{Role: "customer", EntityID: "CUS-T1"})
	event(shipped, "order_shipped_eng2117", at, domain.EventRole{Role: "order", EntityID: "ORD-T1"})
	dev := evidence("DEV-T1", []string{"scope-quality"}, nil)
	observe(dev, "DEV-T1", "severity", "major_eng2117")
	event(dev, "deviation_raised_eng2117", at, domain.EventRole{Role: "deviation", EntityID: "DEV-T1"})

	partyOrders := func(id string) *domain.TagPredicate {
		return &domain.TagPredicate{AnyOfClauses: [][]string{{"scope-orders"}}, PartyScopedTags: []string{"scope-orders"},
			PartyIdentities: []string{id}, ForbiddenTags: []string{"scope-quality"}}
	}
	authorizer := personAuthorizer{byOwner: map[string]*domain.TagPredicate{
		"emp:EMP-T1": partyOrders("emp:EMP-T1"), // the order's sales rep
		"cus:CUS-T1": partyOrders("cus:CUS-T1"), // the order's customer
		"cus:CUS-T2": partyOrders("cus:CUS-T2"), // another account's customer
		"emp:CEO-T":  {AnyOfClauses: [][]string{{"scope-orders", "scope-quality"}}},
		"anon:EXT-T": {ForbiddenTags: []string{"scope-orders", "scope-quality"}}, // a stranger
	}}
	te, err := domain.NewToolExecutor(domain.ToolExecutorControls{Registry: domain.NewInMemoryToolRegistry(),
		Grants: domain.NewInMemoryGrantsStore(), Authz: authorizer}, domain.ToolExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	srv := &subnetwork.Server{ToolExecutor: te}
	wireCompanyRecords(config.DefaultConfig().Execution.Tools.CompanyRecords, srv, sub)

	// On the menu of an agent with no grant (the menu the lane's agent is served).
	menu, err := srv.ListTools(metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", "chat_agent")), &pb.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(menu.GetTools(), func(d *pb.ToolDescriptor) bool { return d.GetName() == records.ToolName }) {
		t.Fatal("company_records is not on the default agent menu")
	}

	run := func(owner, args string) (string, map[string]any) {
		t.Helper()
		c := ctx
		if owner != "" {
			c = domain.WithConversationOwner(ctx, owner)
		}
		out := te.Execute(c, domain.ToolCallRequest{AgentID: "chat_agent", ToolName: records.ToolName, ArgsJSON: []byte(args)})
		if out.Denied || out.Error != "" {
			t.Fatalf("%s %s: denied=%v %s %s", owner, args, out.Denied, out.DenyReason, out.Error)
		}
		var m map[string]any
		if err := json.Unmarshal(out.ResultJSON, &m); err != nil {
			t.Fatalf("result: %v", err)
		}
		return string(out.ResultJSON), m
	}
	getOrder := `{"action":"get","id":"ORD-T1"}`

	for _, owner := range []string{"emp:EMP-T1", "cus:CUS-T1", "emp:CEO-T"} {
		body, m := run(owner, getOrder)
		if m["found"] != true || !strings.Contains(body, "shipped_eng2117") || !strings.Contains(body, "order_shipped_eng2117") ||
			!strings.Contains(body, "order_placed_eng2117") {
			t.Errorf("%s may read the order and did not get it: %s", owner, body)
		}
	}
	// Disallowed: another account's customer, a stranger, the agent for itself.
	// Not found, and nothing of the order — not its status, events or id.
	for _, owner := range []string{"cus:CUS-T2", "anon:EXT-T", ""} {
		for _, args := range []string{getOrder, `{"action":"find","id":"ORD-T"}`, `{"action":"events","id":"ORD-T1"}`,
			`{"action":"find","event_type":"order_shipped_eng2117"}`, `{"action":"types"}`, `{"action":"get","id":"T1"}`} {
			body, m := run(owner, args)
			n, _ := m["count"].(float64)
			if m["found"] == true || n != 0 || m["similar_ids"] != nil || m["record"] != nil || strings.Contains(body, "eng2117") {
				t.Errorf("LEAK to %q on %s: %s", owner, args, body)
			}
		}
	}
	// Deviations: the CEO sees them; the sales rep does not.
	body, m := run("emp:CEO-T", `{"action":"get","id":"DEV-T1"}`)
	if m["found"] != true || !strings.Contains(body, "major_eng2117") || !strings.Contains(body, "deviation_raised_eng2117") {
		t.Errorf("the CEO did not get the deviation: %s", body)
	}
	if body, m := run("emp:EMP-T1", `{"action":"get","id":"DEV-T1"}`); m["found"] == true || strings.Contains(body, "major_eng2117") {
		t.Errorf("LEAK of a deviation to the sales rep: %s", body)
	}
	// Listing by type: the CEO finds the deviation through its event type.
	if body, _ := run("emp:CEO-T", `{"action":"find","event_type":"deviation_raised_eng2117"}`); !strings.Contains(body, "DEV-T1") {
		t.Errorf("find by event type: %s", body)
	}
	// The rep's types list names order things only.
	if body, _ := run("emp:EMP-T1", `{"action":"types"}`); !strings.Contains(body, "order_shipped_eng2117") || strings.Contains(body, "deviation_raised_eng2117") {
		t.Errorf("types for the rep: %s", body)
	}

	// Off by config: not served.
	off := &subnetwork.Server{ToolExecutor: &domain.ToolExecutor{Registry: domain.NewInMemoryToolRegistry(), Grants: domain.NewInMemoryGrantsStore()}}
	wireCompanyRecords(config.CompanyRecordsConfig{Enabled: false}, off, sub)
	if len(off.ToolExecutor.KernelTools) != 0 {
		t.Error("company_records.enabled=false must serve nothing")
	}
}
