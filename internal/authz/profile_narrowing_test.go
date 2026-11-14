package authz

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// profileInner is a by-id store under the decorator.
type profileInner struct {
	domain.VectorStore
	docs map[string]domain.Document
}

func (p profileInner) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := p.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func taggedDoc(id string, tags ...string) domain.Document {
	return domain.Document{ID: id, Metadata: map[string]interface{}{"tags": tags}}
}

// readAs resolves the predicate the way every reader does (ReadFilter), stamps it on
// ctx and reads through the REAL enforcing decorator.
func readAs(t *testing.T, az domain.Authorizer, ctx context.Context, id string) bool {
	t.Helper()
	store := NewEnforcingVectorStore(profileInner{docs: map[string]domain.Document{
		"faq":      taggedDoc("faq", "public"),
		"internal": taggedDoc("internal", "internal"),
		"untagged": {ID: "untagged"},
	}}, nil)
	pred, _ := az.ReadFilter(ctx, domain.AgentPrincipal("chat_agent"), domain.SurfaceRef{Kind: domain.SurfaceChat})
	if pred == nil {
		return false
	}
	d, err := store.GetByID(domain.WithScope(ctx, pred), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return d != nil
}

// ENG-614 (owner decision 2026-11-14): a customer conversation reads only
// customer-visible memory, even over the OSS allow-all (bypass) decision point;
// every other conversation is unchanged.
func TestProfileNarrowing_CustomerReadsOnlyCustomerVisible(t *testing.T) {
	az := NewProfileNarrowingAuthorizer(domain.AllowAllAuthorizer{}, []string{"public"})
	customer := domain.WithConversationProfile(context.Background(), domain.ProfileCustomer)
	if !readAs(t, az, customer, "faq") {
		t.Error("a customer must read a public-tagged row")
	}
	for _, id := range []string{"internal", "untagged"} {
		if readAs(t, az, customer, id) {
			t.Errorf("a customer read %q, which is not customer-visible", id)
		}
	}
	employee := domain.WithConversationProfile(context.Background(), domain.ProfileEmployee)
	for _, ctx := range []context.Context{employee, context.Background()} {
		for _, id := range []string{"faq", "internal", "untagged"} {
			if !readAs(t, az, ctx, id) {
				t.Errorf("a non-customer read of %q was narrowed", id)
			}
		}
	}
}

// No customer-visible tag configured ⇒ a customer conversation reads nothing (fail
// closed); an inner refusal stays a refusal.
func TestProfileNarrowing_FailsClosed(t *testing.T) {
	customer := domain.WithConversationProfile(context.Background(), domain.ProfileCustomer)
	if pred, _ := NewProfileNarrowingAuthorizer(domain.AllowAllAuthorizer{}, nil).ReadFilter(customer, domain.AgentPrincipal("a"), domain.SurfaceRef{}); pred != nil {
		t.Fatalf("empty customer_visible_tags must grant no read, got %+v", pred)
	}
	az := NewProfileNarrowingAuthorizer(noReadAuthorizer{}, []string{"public"})
	if pred, _ := az.ReadFilter(customer, domain.AgentPrincipal("a"), domain.SurfaceRef{}); pred != nil {
		t.Fatalf("an inner refusal must stay a refusal, got %+v", pred)
	}
}

type noReadAuthorizer struct{ domain.AllowAllAuthorizer }

func (noReadAuthorizer) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: domain.ReasonNoPrincipal}
}
