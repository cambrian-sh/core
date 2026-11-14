package operator_test

import (
	"context"
	"sort"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
)

// ENG-666, ADR-0142 migration step 2: a person's audit rows were recorded under
// several actor strings — the bare id (operator RPCs), user:<id>, and the
// pre-ADR-0142 agent:<id> alias. A query by any one form must match all of them,
// and must not pull in another person's rows.
func TestQueryAudit_MatchesEveryRecordedFormOfAPerson(t *testing.T) {
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	for _, id := range []string{"alice", "bob"} {
		if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := reg.AddAlias(ctx, domain.LegacyAgentAlias(id), id); err != nil {
			t.Fatal(err)
		}
	}
	svc, _, audit, _ := newCommandService()
	svc.SetPrincipalNames(reg)
	at := time.Now()
	for i, actor := range []string{"alice", "user:alice", "agent:alice", "bob", "agent:bob", "agent:some-bot"} {
		if _, err := audit.Record(ctx, domain.AuditEntry{ID: actor, CommandID: actor, Actor: actor,
			At: at.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	actors := func(req string) []string {
		t.Helper()
		resp, err := svc.QueryAudit(ctx, &pb.QueryAuditRequest{Actor: req})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetActor())
		}
		sort.Strings(out)
		return out
	}
	want := []string{"agent:alice", "alice", "user:alice"}
	for _, q := range []string{"alice", "user:alice", "agent:alice"} {
		if got := actors(q); !equalStrings(got, want) {
			t.Errorf("query %q matched %v, want %v (before ENG-666 only the exact string matched)", q, got, want)
		}
	}
	// A non-person actor keeps exact matching.
	if got := actors("agent:some-bot"); !equalStrings(got, []string{"agent:some-bot"}) {
		t.Errorf("an agent's own rows: %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
